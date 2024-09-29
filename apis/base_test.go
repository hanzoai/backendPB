package apis_test

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/router"
)

func TestWrapStdHandler(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	e := new(core.RequestEvent)
	e.App = app
	e.Request = req
	e.Response = rec

	err := apis.WrapStdHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("test"))
	}))(e)
	if err != nil {
		t.Fatal(err)
	}

	if body := rec.Body.String(); body != "test" {
		t.Fatalf("Expected body %q, got %q", "test", body)
	}
}

func TestWrapStdMiddleware(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	e := new(core.RequestEvent)
	e.App = app
	e.Request = req
	e.Response = rec

	err := apis.WrapStdMiddleware(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("test"))
		})
	})(e)
	if err != nil {
		t.Fatal(err)
	}

	if body := rec.Body.String(); body != "test" {
		t.Fatalf("Expected body %q, got %q", "test", body)
	}
}

func TestStatic(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	dir := createTestDir(t)
	defer os.RemoveAll(dir)

	fsys := os.DirFS(filepath.Join(dir, "sub"))

	type staticScenario struct {
		path           string
		indexFallback  bool
		expectedStatus int
		expectBody     string
		expectError    bool
	}

	scenarios := []staticScenario{
		{
			path:           "",
			indexFallback:  false,
			expectedStatus: 200,
			expectBody:     "sub index.html",
			expectError:    false,
		},
		{
			path:           "missing/a/b/c",
			indexFallback:  false,
			expectedStatus: 404,
			expectBody:     "",
			expectError:    true,
		},
		{
			path:           "missing/a/b/c",
			indexFallback:  true,
			expectedStatus: 200,
			expectBody:     "sub index.html",
			expectError:    false,
		},
		{
			path:           "testroot", // parent directory file
			indexFallback:  false,
			expectedStatus: 404,
			expectBody:     "",
			expectError:    true,
		},
		{
			path:           "test",
			indexFallback:  false,
			expectedStatus: 200,
			expectBody:     "sub test",
			expectError:    false,
		},
		{
			path:           "sub2",
			indexFallback:  false,
			expectedStatus: 301,
			expectBody:     "",
			expectError:    false,
		},
		{
			path:           "sub2/",
			indexFallback:  false,
			expectedStatus: 200,
			expectBody:     "sub2 index.html",
			expectError:    false,
		},
		{
			path:           "sub2/test",
			indexFallback:  false,
			expectedStatus: 200,
			expectBody:     "sub2 test",
			expectError:    false,
		},
		{
			path:           "sub2/test/",
			indexFallback:  false,
			expectedStatus: 301,
			expectBody:     "",
			expectError:    false,
		},
	}

	// extra directory traversal checks
	dtp := []string{
		"/../",
		"\\../",
		"../",
		"../../",
		"..\\",
		"..\\..\\",
		"../..\\",
		"..\\..//",
		`%2e%2e%2f`,
		`%2e%2e%2f%2e%2e%2f`,
		`%2e%2e/`,
		`%2e%2e/%2e%2e/`,
		`..%2f`,
		`..%2f..%2f`,
		`%2e%2e%5c`,
		`%2e%2e%5c%2e%2e%5c`,
		`%2e%2e\`,
		`%2e%2e\%2e%2e\`,
		`..%5c`,
		`..%5c..%5c`,
		`%252e%252e%255c`,
		`%252e%252e%255c%252e%252e%255c`,
		`..%255c`,
		`..%255c..%255c`,
	}
	for _, p := range dtp {
		scenarios = append(scenarios,
			staticScenario{
				path:           p + "testroot",
				indexFallback:  false,
				expectedStatus: 404,
				expectBody:     "",
				expectError:    true,
			},
			staticScenario{
				path:           p + "testroot",
				indexFallback:  true,
				expectedStatus: 200,
				expectBody:     "sub index.html",
				expectError:    false,
			},
		)
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%s_%v", i, s.path, s.indexFallback), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/"+s.path, nil)
			req.SetPathValue(apis.StaticWildcardParam, s.path)

			rec := httptest.NewRecorder()

			e := new(core.RequestEvent)
			e.App = app
			e.Request = req
			e.Response = rec

			err := apis.Static(fsys, s.indexFallback)(e)

			hasErr := err != nil
			if hasErr != s.expectError {
				t.Fatalf("Expected hasErr %v, got %v (%v)", s.expectError, hasErr, err)
			}

			body := rec.Body.String()
			if body != s.expectBody {
				t.Fatalf("Expected body %q, got %q", s.expectBody, body)
			}

			if hasErr {
				apiErr := router.ToApiError(err)
				if apiErr.Status != s.expectedStatus {
					t.Fatalf("Expected status code %d, got %d", s.expectedStatus, apiErr.Status)
				}
			}
		})
	}
}

func TestFindUploadedFiles(t *testing.T) {
	scenarios := []struct {
		filename        string
		expectedPattern string
	}{
		{"ab.png", `^ab\w{10}_\w{10}\.png$`},
		{"test", `^test_\w{10}\.txt$`},
		{"a b c d!@$.j!@$pg", `^a_b_c_d_\w{10}\.jpg$`},
		{strings.Repeat("a", 150), `^a{100}_\w{10}\.txt$`},
	}

	for _, s := range scenarios {
		t.Run(s.filename, func(t *testing.T) {
			// create multipart form file body
			body := new(bytes.Buffer)
			mp := multipart.NewWriter(body)
			w, err := mp.CreateFormFile("test", s.filename)
			if err != nil {
				t.Fatal(err)
			}
			w.Write([]byte("test"))
			mp.Close()
			// ---

			req := httptest.NewRequest(http.MethodPost, "/", body)
			req.Header.Add("Content-Type", mp.FormDataContentType())

			result, err := apis.FindUploadedFiles(req, "test")
			if err != nil {
				t.Fatal(err)
			}

			if len(result) != 1 {
				t.Fatalf("Expected 1 file, got %d", len(result))
			}

			if result[0].Size != 4 {
				t.Fatalf("Expected the file size to be 4 bytes, got %d", result[0].Size)
			}

			pattern, err := regexp.Compile(s.expectedPattern)
			if err != nil {
				t.Fatalf("Invalid filename pattern %q: %v", s.expectedPattern, err)
			}
			if !pattern.MatchString(result[0].Name) {
				t.Fatalf("Expected filename to match %s, got filename %s", s.expectedPattern, result[0].Name)
			}
		})
	}
}

func TestFindUploadedFilesMissing(t *testing.T) {
	body := new(bytes.Buffer)
	mp := multipart.NewWriter(body)
	mp.Close()

	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.Header.Add("Content-Type", mp.FormDataContentType())

	result, err := apis.FindUploadedFiles(req, "test")
	if err == nil {
		t.Error("Expected error, got nil")
	}

	if result != nil {
		t.Errorf("Expected result to be nil, got %v", result)
	}
}

func TestMustSubFS(t *testing.T) {
	t.Parallel()

	dir := createTestDir(t)
	defer os.RemoveAll(dir)

	// invalid path (no beginning and ending slashes)
	if !hasPanicked(func() {
		apis.MustSubFS(os.DirFS(dir), "/test/")
	}) {
		t.Fatalf("Expected to panic")
	}

	// valid path
	if hasPanicked(func() {
		apis.MustSubFS(os.DirFS(dir), "./////a/b/c") // checks if ToSlash was called
	}) {
		t.Fatalf("Didn't expect to panic")
	}

	// check sub content
	sub := apis.MustSubFS(os.DirFS(dir), "sub")

	_, err := sub.Open("test")
	if err != nil {
		t.Fatalf("Missing expected file sub/test")
	}
}

// -------------------------------------------------------------------

func hasPanicked(f func()) (didPanic bool) {
	defer func() {
		if r := recover(); r != nil {
			didPanic = true
		}
	}()
	f()
	return
}

// note: make sure to call os.RemoveAll(dir) after you are done
// working with the created test dir.
func createTestDir(t *testing.T) string {
	dir, err := os.MkdirTemp(os.TempDir(), "test_dir")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("root index.html"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testroot"), []byte("root test"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "sub"), os.ModePerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub/index.html"), []byte("sub index.html"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub/test"), []byte("sub test"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "sub", "sub2"), os.ModePerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub/sub2/index.html"), []byte("sub2 index.html"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub/sub2/test"), []byte("sub2 test"), 0644); err != nil {
		t.Fatal(err)
	}

	return dir
}
