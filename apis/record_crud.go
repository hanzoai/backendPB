package apis

import (
	cryptoRand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/hanzoai/backendPB/core"
	"github.com/hanzoai/backendPB/forms"
	"github.com/hanzoai/backendPB/tools/filesystem"
	"github.com/hanzoai/backendPB/tools/inflector"
	"github.com/hanzoai/backendPB/tools/list"
	"github.com/hanzoai/backendPB/tools/router"
	"github.com/hanzoai/backendPB/tools/search"
	"github.com/hanzoai/backendPB/tools/security"
	"github.com/hanzoai/dbx"
)

// bindRecordCrudApi registers the record crud api endpoints and
// the corresponding handlers.
//
// note: the rate limiter is "inlined" because some of the crud actions are also used in the batch APIs
func bindRecordCrudApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	subGroup := rg.Group("/collections/{collection}/records").Unbind(DefaultRateLimitMiddlewareId)
	subGroup.GET("", recordsList)
	subGroup.GET("/{id}", recordView)
	subGroup.POST("", recordCreate(nil)).Bind(dynamicCollectionBodyLimit(""))
	subGroup.PATCH("/{id}", recordUpdate(nil)).Bind(dynamicCollectionBodyLimit(""))
	subGroup.DELETE("/{id}", recordDelete(nil))
}

func recordsList(e *core.RequestEvent) error {
	collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || collection == nil {
		return e.NotFoundError("Missing collection context.", err)
	}

	err = checkCollectionRateLimit(e, collection, "list")
	if err != nil {
		return err
	}

	requestInfo, err := e.RequestInfo()
	if err != nil {
		return firstApiError(err, e.BadRequestError("", err))
	}

	if collection.ListRule == nil && !requestInfo.HasSuperuserAuth() {
		return e.ForbiddenError("Only superusers can perform this action.", nil)
	}

	// forbid users and guests to query special filter/sort fields
	err = checkForSuperuserOnlyRuleFields(requestInfo)
	if err != nil {
		return err
	}

	query := e.App.RecordQuery(collection)

	fieldsResolver := core.NewRecordFieldResolver(e.App, collection, requestInfo, true)

	if !requestInfo.HasSuperuserAuth() && collection.ListRule != nil && *collection.ListRule != "" {
		expr, err := search.FilterData(*collection.ListRule).BuildExpr(fieldsResolver)
		if err != nil {
			return err
		}
		query.AndWhere(expr)

		// will be applied by the search provider right before executing the query
		// fieldsResolver.UpdateQuery(query)
	}

	// hidden fields are searchable only by superusers
	fieldsResolver.SetAllowHiddenFields(requestInfo.HasSuperuserAuth())

	searchProvider := search.NewProvider(fieldsResolver).Query(query)

	records := []*core.Record{}
	result, err := searchProvider.ParseAndExec(e.Request.URL.Query().Encode(), &records)
	if err != nil {
		return firstApiError(err, e.BadRequestError("", err))
	}

	event := new(core.RecordsListRequestEvent)
	event.RequestEvent = e
	event.Collection = collection
	event.Records = records
	event.Result = result

	return e.App.OnRecordsListRequest().Trigger(event, func(e *core.RecordsListRequestEvent) error {
		if err := EnrichRecords(e.RequestEvent, e.Records); err != nil {
			return firstApiError(err, e.InternalServerError("Failed to enrich records", err))
		}

		// Add a randomized throttle in case of too many empty search filter attempts.
		//
		// This is just for extra precaution since security researches raised concern regarding the possibility of eventual
		// timing attacks because the List API rule acts also as filter and executes in a single run with the client-side filters.
		// This is by design and it is an accepted trade off between performance, usability and correctness.
		//
		// While technically the below doesn't fully guarantee protection against filter timing attacks, in practice combined with the network latency it makes them even less feasible.
		// A properly configured rate limiter or individual fields Hidden checks are better suited if you are really concerned about eventual information disclosure by side-channel attacks.
		//
		// In all cases it doesn't really matter that much because it doesn't affect the builtin HanzoBase security sensitive fields (e.g. password and tokenKey) since they
		// are not client-side filterable and in the few places where they need to be compared against an external value, a constant time check is used.
		if !e.HasSuperuserAuth() &&
			(collection.ListRule != nil && *collection.ListRule != "") &&
			(requestInfo.Query["filter"] != "") &&
			len(e.Records) == 0 &&
			checkRateLimit(e.RequestEvent, "@hb_list_timing_check_"+collection.Id, listTimingRateLimitRule) != nil {
			e.App.Logger().Debug("Randomized throttle because of too many failed searches", "collectionId", collection.Id)
			randomizedThrottle(150)
		}

		return e.JSON(http.StatusOK, e.Result)
	})
}

var listTimingRateLimitRule = core.RateLimitRule{MaxRequests: 3, Duration: 3}

func randomizedThrottle(softMax int64) {
	var timeout int64
	randRange, err := cryptoRand.Int(cryptoRand.Reader, big.NewInt(softMax))
	if err == nil {
		timeout = randRange.Int64()
	} else {
		timeout = softMax
	}

	time.Sleep(time.Duration(timeout) * time.Millisecond)
}

func recordView(e *core.RequestEvent) error {
	collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || collection == nil {
		return e.NotFoundError("Missing collection context.", err)
	}

	err = checkCollectionRateLimit(e, collection, "view")
	if err != nil {
		return err
	}

	recordId := e.Request.PathValue("id")
	if recordId == "" {
		return e.NotFoundError("", nil)
	}

	requestInfo, err := e.RequestInfo()
	if err != nil {
		return firstApiError(err, e.BadRequestError("", err))
	}

	if collection.ViewRule == nil && !requestInfo.HasSuperuserAuth() {
		return e.ForbiddenError("Only superusers can perform this action.", nil)
	}

	ruleFunc := func(q *dbx.SelectQuery) error {
		if !requestInfo.HasSuperuserAuth() && collection.ViewRule != nil && *collection.ViewRule != "" {
			resolver := core.NewRecordFieldResolver(e.App, collection, requestInfo, true)
			expr, err := search.FilterData(*collection.ViewRule).BuildExpr(resolver)
			if err != nil {
				return err
			}
			resolver.UpdateQuery(q)
			q.AndWhere(expr)
		}
		return nil
	}

	record, fetchErr := e.App.FindRecordById(collection, recordId, ruleFunc)
	if fetchErr != nil || record == nil {
		return firstApiError(err, e.NotFoundError("", fetchErr))
	}

	event := new(core.RecordRequestEvent)
	event.RequestEvent = e
	event.Collection = collection
	event.Record = record

	return e.App.OnRecordViewRequest().Trigger(event, func(e *core.RecordRequestEvent) error {
		if err := EnrichRecord(e.RequestEvent, e.Record); err != nil {
			return firstApiError(err, e.InternalServerError("Failed to enrich record", err))
		}

		return e.JSON(http.StatusOK, e.Record)
	})
}

func recordCreate(optFinalizer func(data any) error) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
		if err != nil || collection == nil {
			return e.NotFoundError("Missing collection context.", err)
		}

		if collection.IsView() {
			return e.BadRequestError("Unsupported collection type.", nil)
		}

		err = checkCollectionRateLimit(e, collection, "create")
		if err != nil {
			return err
		}

		requestInfo, err := e.RequestInfo()
		if err != nil {
			return firstApiError(err, e.BadRequestError("", err))
		}

		hasSuperuserAuth := requestInfo.HasSuperuserAuth()
		if !hasSuperuserAuth && collection.CreateRule == nil {
			return e.ForbiddenError("Only superusers can perform this action.", nil)
		}

		record := core.NewRecord(collection)

		data, err := recordDataFromRequest(e, record)
		if err != nil {
			return firstApiError(err, e.BadRequestError("Failed to read the submitted data.", err))
		}

		// set a random password for the OAuth2 ignoring its plain password validators
		var skipPlainPasswordRecordValidators bool
		if requestInfo.Context == core.RequestInfoContextOAuth2 {
			if _, ok := data[core.FieldNamePassword]; !ok {
				data[core.FieldNamePassword] = security.RandomString(30)
				data[core.FieldNamePassword+"Confirm"] = data[core.FieldNamePassword]
				skipPlainPasswordRecordValidators = true
			}
		}

		// replace modifiers fields so that the resolved value is always
		// available when accessing requestInfo.Body
		requestInfo.Body = data

		form := forms.NewRecordUpsert(e.App, record)
		if hasSuperuserAuth {
			form.GrantSuperuserAccess()
		}
		form.Load(data)

		if skipPlainPasswordRecordValidators {
			// unset the plain value to skip the plain password field validators
			if raw, ok := record.GetRaw(core.FieldNamePassword).(*core.PasswordFieldValue); ok {
				raw.Plain = ""
			}
		}

		var isOptFinalizerCalled bool

		event := new(core.RecordRequestEvent)
		event.RequestEvent = e
		event.Collection = collection
		event.Record = record

		hookErr := e.App.OnRecordCreateRequest().Trigger(event, func(e *core.RecordRequestEvent) error {
			form.SetApp(e.App)
			form.SetRecord(e.Record)

			// temporary save the record and check it against the create and manage rules
			if !hasSuperuserAuth && e.Collection.CreateRule != nil {
				dummyRecord := e.Record.Clone()

				dummyRandomPart := "__hb_create__" + security.PseudorandomString(6)

				// set an id if it doesn't have already
				// (the value doesn't matter; it is used only to minimize the breaking changes with earlier versions)
				if dummyRecord.Id == "" {
					dummyRecord.Id = "__temp_id__" + dummyRandomPart
				}

				// unset the verified field to prevent manage API rule misuse in case the rule relies on it
				dummyRecord.SetVerified(false)

				// export the dummy record data into db params
				dummyExport, err := dummyRecord.DBExport(e.App)
				if err != nil {
					return e.BadRequestError("Failed to create record", fmt.Errorf("dummy DBExport error: %w", err))
				}

				dummyParams := make(dbx.Params, len(dummyExport))
				selects := make([]string, 0, len(dummyExport))
				var param string
				for k, v := range dummyExport {
					k = inflector.Columnify(k) // columnify is just as extra measure in case of custom fields
					param = "__hb_create__" + k
					dummyParams[param] = v
					selects = append(selects, "{:"+param+"} AS [["+k+"]]")
				}

				// shallow clone the current collection
				dummyCollection := *e.Collection
				dummyCollection.Id += dummyRandomPart
				dummyCollection.Name += inflector.Columnify(dummyRandomPart)

				withFrom := fmt.Sprintf("WITH {{%s}} as (SELECT %s)", dummyCollection.Name, strings.Join(selects, ","))

				// check non-empty create rule
				if *dummyCollection.CreateRule != "" {
					ruleQuery := e.App.DB().Select("(1)").PreFragment(withFrom).From(dummyCollection.Name).AndBind(dummyParams)

					resolver := core.NewRecordFieldResolver(e.App, &dummyCollection, requestInfo, true)

					expr, err := search.FilterData(*dummyCollection.CreateRule).BuildExpr(resolver)
					if err != nil {
						return e.BadRequestError("Failed to create record", fmt.Errorf("create rule build expression failure: %w", err))
					}
					ruleQuery.AndWhere(expr)

					resolver.UpdateQuery(ruleQuery)

					var exists bool
					err = ruleQuery.Limit(1).Row(&exists)
					if err != nil || !exists {
						return e.BadRequestError("Failed to create record", fmt.Errorf("create rule failure: %w", err))
					}
				}

				// check for manage rule access
				manageRuleQuery := e.App.DB().Select("(1)").PreFragment(withFrom).From(dummyCollection.Name).AndBind(dummyParams)
				if !form.HasManageAccess() &&
					hasAuthManageAccess(e.App, requestInfo, &dummyCollection, manageRuleQuery) {
					form.GrantManagerAccess()
				}
			}

			err := form.Submit()
			if err != nil {
				return firstApiError(err, e.BadRequestError("Failed to create record", err))
			}

			err = EnrichRecord(e.RequestEvent, e.Record)
			if err != nil {
				return firstApiError(err, e.InternalServerError("Failed to enrich record", err))
			}

			err = e.JSON(http.StatusOK, e.Record)
			if err != nil {
				return err
			}

			if optFinalizer != nil {
				isOptFinalizerCalled = true
				err = optFinalizer(e.Record)
				if err != nil {
					return firstApiError(err, e.InternalServerError("", err))
				}
			}

			return nil
		})
		if hookErr != nil {
			return hookErr
		}

		// e.g. in case the regular hook chain was stopped and the finalizer cannot be executed as part of the last e.Next() task
		if !isOptFinalizerCalled && optFinalizer != nil {
			if err := optFinalizer(event.Record); err != nil {
				return firstApiError(err, e.InternalServerError("", err))
			}
		}

		return nil
	}
}

func recordUpdate(optFinalizer func(data any) error) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
		if err != nil || collection == nil {
			return e.NotFoundError("Missing collection context.", err)
		}

		if collection.IsView() {
			return e.BadRequestError("Unsupported collection type.", nil)
		}

		err = checkCollectionRateLimit(e, collection, "update")
		if err != nil {
			return err
		}

		recordId := e.Request.PathValue("id")
		if recordId == "" {
			return e.NotFoundError("", nil)
		}

		requestInfo, err := e.RequestInfo()
		if err != nil {
			return firstApiError(err, e.BadRequestError("", err))
		}

		hasSuperuserAuth := requestInfo.HasSuperuserAuth()

		if !hasSuperuserAuth && collection.UpdateRule == nil {
			return firstApiError(err, e.ForbiddenError("Only superusers can perform this action.", nil))
		}

		// eager fetch the record so that the modifiers field values can be resolved
		record, err := e.App.FindRecordById(collection, recordId)
		if err != nil {
			return firstApiError(err, e.NotFoundError("", err))
		}

		data, err := recordDataFromRequest(e, record)
		if err != nil {
			return firstApiError(err, e.BadRequestError("Failed to read the submitted data.", err))
		}

		// replace modifiers fields so that the resolved value is always
		// available when accessing requestInfo.Body
		requestInfo.Body = data

		ruleFunc := func(q *dbx.SelectQuery) error {
			if !hasSuperuserAuth && collection.UpdateRule != nil && *collection.UpdateRule != "" {
				resolver := core.NewRecordFieldResolver(e.App, collection, requestInfo, true)
				expr, err := search.FilterData(*collection.UpdateRule).BuildExpr(resolver)
				if err != nil {
					return err
				}
				resolver.UpdateQuery(q)
				q.AndWhere(expr)
			}
			return nil
		}

		// refetch with access checks
		record, err = e.App.FindRecordById(collection, recordId, ruleFunc)
		if err != nil {
			return firstApiError(err, e.NotFoundError("", err))
		}

		form := forms.NewRecordUpsert(e.App, record)
		if hasSuperuserAuth {
			form.GrantSuperuserAccess()
		}
		form.Load(data)

		var isOptFinalizerCalled bool

		event := new(core.RecordRequestEvent)
		event.RequestEvent = e
		event.Collection = collection
		event.Record = record

		hookErr := e.App.OnRecordUpdateRequest().Trigger(event, func(e *core.RecordRequestEvent) error {
			form.SetApp(e.App)
			form.SetRecord(e.Record)

			manageRuleQuery := e.App.DB().Select("(1)").From(e.Collection.Name).AndWhere(dbx.HashExp{
				e.Collection.Name + ".id": e.Record.Id,
			})
			if !form.HasManageAccess() &&
				hasAuthManageAccess(e.App, requestInfo, e.Collection, manageRuleQuery) {
				form.GrantManagerAccess()
			}

			err := form.Submit()
			if err != nil {
				return firstApiError(err, e.BadRequestError("Failed to update record.", err))
			}

			err = EnrichRecord(e.RequestEvent, e.Record)
			if err != nil {
				return firstApiError(err, e.InternalServerError("Failed to enrich record", err))
			}

			err = e.JSON(http.StatusOK, e.Record)
			if err != nil {
				return err
			}

			if optFinalizer != nil {
				isOptFinalizerCalled = true
				err = optFinalizer(e.Record)
				if err != nil {
					return firstApiError(err, e.InternalServerError("", fmt.Errorf("update optFinalizer error: %w", err)))
				}
			}

			return nil
		})
		if hookErr != nil {
			return hookErr
		}

		// e.g. in case the regular hook chain was stopped and the finalizer cannot be executed as part of the last e.Next() task
		if !isOptFinalizerCalled && optFinalizer != nil {
			if err := optFinalizer(event.Record); err != nil {
				return firstApiError(err, e.InternalServerError("", fmt.Errorf("update optFinalizer error: %w", err)))
			}
		}

		return nil
	}
}

func recordDelete(optFinalizer func(data any) error) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
		if err != nil || collection == nil {
			return e.NotFoundError("Missing collection context.", err)
		}

		if collection.IsView() {
			return e.BadRequestError("Unsupported collection type.", nil)
		}

		err = checkCollectionRateLimit(e, collection, "delete")
		if err != nil {
			return err
		}

		recordId := e.Request.PathValue("id")
		if recordId == "" {
			return e.NotFoundError("", nil)
		}

		requestInfo, err := e.RequestInfo()
		if err != nil {
			return firstApiError(err, e.BadRequestError("", err))
		}

		if !requestInfo.HasSuperuserAuth() && collection.DeleteRule == nil {
			return e.ForbiddenError("Only superusers can perform this action.", nil)
		}

		ruleFunc := func(q *dbx.SelectQuery) error {
			if !requestInfo.HasSuperuserAuth() && collection.DeleteRule != nil && *collection.DeleteRule != "" {
				resolver := core.NewRecordFieldResolver(e.App, collection, requestInfo, true)
				expr, err := search.FilterData(*collection.DeleteRule).BuildExpr(resolver)
				if err != nil {
					return err
				}
				resolver.UpdateQuery(q)
				q.AndWhere(expr)
			}
			return nil
		}

		record, err := e.App.FindRecordById(collection, recordId, ruleFunc)
		if err != nil || record == nil {
			return e.NotFoundError("", err)
		}

		var isOptFinalizerCalled bool

		event := new(core.RecordRequestEvent)
		event.RequestEvent = e
		event.Collection = collection
		event.Record = record

		hookErr := e.App.OnRecordDeleteRequest().Trigger(event, func(e *core.RecordRequestEvent) error {
			if err := e.App.Delete(e.Record); err != nil {
				return firstApiError(err, e.BadRequestError("Failed to delete record. Make sure that the record is not part of a required relation reference.", err))
			}

			err = e.NoContent(http.StatusNoContent)
			if err != nil {
				return err
			}

			if optFinalizer != nil {
				isOptFinalizerCalled = true
				err = optFinalizer(e.Record)
				if err != nil {
					return firstApiError(err, e.InternalServerError("", fmt.Errorf("delete optFinalizer error: %w", err)))
				}
			}

			return nil
		})
		if hookErr != nil {
			return hookErr
		}

		// e.g. in case the regular hook chain was stopped and the finalizer cannot be executed as part of the last e.Next() task
		if !isOptFinalizerCalled && optFinalizer != nil {
			if err := optFinalizer(event.Record); err != nil {
				return firstApiError(err, e.InternalServerError("", fmt.Errorf("delete optFinalizer error: %w", err)))
			}
		}

		return nil
	}
}

// -------------------------------------------------------------------

func recordDataFromRequest(e *core.RequestEvent, record *core.Record) (map[string]any, error) {
	info, err := e.RequestInfo()
	if err != nil {
		return nil, err
	}

	// resolve regular fields
	result := record.ReplaceModifiers(info.Body)

	// resolve uploaded files
	uploadedFiles, err := extractUploadedFiles(e, record.Collection(), "")
	if err != nil {
		return nil, err
	}
	if len(uploadedFiles) > 0 {
		for k, files := range uploadedFiles {
			uploaded := make([]any, 0, len(files))

			// if not remove/prepend/append -> merge with the submitted
			// info.Body values to prevent accidental old files deletion
			if info.Body[k] != nil &&
				!strings.HasPrefix(k, "+") &&
				!strings.HasSuffix(k, "+") &&
				!strings.HasSuffix(k, "-") {
				existing := list.ToUniqueStringSlice(info.Body[k])
				for _, name := range existing {
					uploaded = append(uploaded, name)
				}
			}

			for _, file := range files {
				uploaded = append(uploaded, file)
			}

			result[k] = uploaded
		}

		result = record.ReplaceModifiers(result)
	}

	isAuth := record.Collection().IsAuth()

	// unset hidden fields for non-superusers
	if !info.HasSuperuserAuth() {
		for _, f := range record.Collection().Fields {
			if f.GetHidden() {
				// exception for the auth collection "password" field
				if isAuth && f.GetName() == core.FieldNamePassword {
					continue
				}

				delete(result, f.GetName())
			}
		}
	}

	return result, nil
}

func extractUploadedFiles(re *core.RequestEvent, collection *core.Collection, prefix string) (map[string][]*filesystem.File, error) {
	contentType := re.Request.Header.Get("content-type")
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		return nil, nil // not multipart/form-data request
	}

	result := map[string][]*filesystem.File{}

	for _, field := range collection.Fields {
		if field.Type() != core.FieldTypeFile {
			continue
		}

		baseKey := field.GetName()

		keys := []string{
			baseKey,
			// prepend and append modifiers
			"+" + baseKey,
			baseKey + "+",
		}

		for _, k := range keys {
			if prefix != "" {
				k = prefix + "." + k
			}
			files, err := re.FindUploadedFiles(k)
			if err != nil && !errors.Is(err, http.ErrMissingFile) {
				return nil, err
			}
			if len(files) > 0 {
				result[k] = files
			}
		}
	}

	return result, nil
}

// hasAuthManageAccess checks whether the client is allowed to have
// [forms.RecordUpsert] auth management permissions
// (e.g. allowing to change system auth fields without oldPassword).
func hasAuthManageAccess(app core.App, requestInfo *core.RequestInfo, collection *core.Collection, query *dbx.SelectQuery) bool {
	if !collection.IsAuth() {
		return false
	}

	manageRule := collection.ManageRule

	if manageRule == nil || *manageRule == "" {
		return false // only for superusers (manageRule can't be empty)
	}

	if requestInfo == nil || requestInfo.Auth == nil {
		return false // no auth record
	}

	resolver := core.NewRecordFieldResolver(app, collection, requestInfo, true)

	expr, err := search.FilterData(*manageRule).BuildExpr(resolver)
	if err != nil {
		app.Logger().Error("Manage rule build expression error", "error", err, "collectionId", collection.Id)
		return false
	}
	query.AndWhere(expr)

	resolver.UpdateQuery(query)

	var exists bool

	err = query.Limit(1).Row(&exists)

	return err == nil && exists
}
