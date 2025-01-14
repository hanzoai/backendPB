package hanzobase

import (
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/hanzoai/backendPB/cmd"
	"github.com/hanzoai/backendPB/core"
	"github.com/hanzoai/backendPB/tools/hook"
	"github.com/hanzoai/backendPB/tools/list"
	"github.com/hanzoai/backendPB/tools/routine"
	"github.com/spf13/cobra"

	_ "github.com/hanzoai/backendPB/migrations"
)

var _ core.App = (*HanzoBase)(nil)

// Version of HanzoBase
var Version = "(untracked)"

// HanzoBase defines a HanzoBase app launcher.
//
// It implements [core.App] via embedding and all of the app interface methods
// could be accessed directly through the instance (eg. HanzoBase.DataDir()).
type HanzoBase struct {
	core.App

	devFlag           bool
	dataDirFlag       string
	encryptionEnvFlag string
	queryTimeout      int
	hideStartBanner   bool

	// RootCmd is the main console command
	RootCmd *cobra.Command
}

// Config is the HanzoBase initialization config struct.
type Config struct {
	// hide the default console server info on app startup
	HideStartBanner bool

	// optional default values for the console flags
	DefaultDev           bool
	DefaultDataDir       string // if not set, it will fallback to "./hb_data"
	DefaultEncryptionEnv string
	DefaultQueryTimeout  time.Duration // default to core.DefaultQueryTimeout (in seconds)

	// optional DB configurations
	DataMaxOpenConns int                // default to core.DefaultDataMaxOpenConns
	DataMaxIdleConns int                // default to core.DefaultDataMaxIdleConns
	AuxMaxOpenConns  int                // default to core.DefaultAuxMaxOpenConns
	AuxMaxIdleConns  int                // default to core.DefaultAuxMaxIdleConns
	DBConnect        core.DBConnectFunc // default to core.dbConnect
}

// New creates a new HanzoBase instance with the default configuration.
// Use [NewWithConfig] if you want to provide a custom configuration.
//
// Note that the application will not be initialized/bootstrapped yet,
// aka. DB connections, migrations, app settings, etc. will not be accessible.
// Everything will be initialized when [HanzoBase.Start] is executed.
// If you want to initialize the application before calling [HanzoBase.Start],
// then you'll have to manually call [HanzoBase.Bootstrap].
func New() *HanzoBase {
	_, isUsingGoRun := inspectRuntime()

	return NewWithConfig(Config{
		DefaultDev: isUsingGoRun,
	})
}

// NewWithConfig creates a new HanzoBase instance with the provided config.
//
// Note that the application will not be initialized/bootstrapped yet,
// aka. DB connections, migrations, app settings, etc. will not be accessible.
// Everything will be initialized when [HanzoBase.Start] is executed.
// If you want to initialize the application before calling [HanzoBase.Start],
// then you'll have to manually call [HanzoBase.Bootstrap].
func NewWithConfig(config Config) *HanzoBase {
	// initialize a default data directory based on the executable baseDir
	if config.DefaultDataDir == "" {
		baseDir, _ := inspectRuntime()
		config.DefaultDataDir = filepath.Join(baseDir, "hb_data")
	}

	if config.DefaultQueryTimeout == 0 {
		config.DefaultQueryTimeout = core.DefaultQueryTimeout
	}

	executableName := filepath.Base(os.Args[0])

	hb := &HanzoBase{
		RootCmd: &cobra.Command{
			Use:     executableName,
			Short:   executableName + " CLI",
			Version: Version,
			FParseErrWhitelist: cobra.FParseErrWhitelist{
				UnknownFlags: true,
			},
			// no need to provide the default cobra completion command
			CompletionOptions: cobra.CompletionOptions{
				DisableDefaultCmd: true,
			},
		},
		devFlag:           config.DefaultDev,
		dataDirFlag:       config.DefaultDataDir,
		encryptionEnvFlag: config.DefaultEncryptionEnv,
		hideStartBanner:   config.HideStartBanner,
	}

	// replace with a colored stderr writer
	hb.RootCmd.SetErr(newErrWriter())

	// parse base flags
	// (errors are ignored, since the full flags parsing happens on Execute())
	hb.eagerParseFlags(&config)

	// initialize the app instance
	hb.App = core.NewBaseApp(core.BaseAppConfig{
		IsDev:            hb.devFlag,
		DataDir:          hb.dataDirFlag,
		EncryptionEnv:    hb.encryptionEnvFlag,
		QueryTimeout:     time.Duration(hb.queryTimeout) * time.Second,
		DataMaxOpenConns: config.DataMaxOpenConns,
		DataMaxIdleConns: config.DataMaxIdleConns,
		AuxMaxOpenConns:  config.AuxMaxOpenConns,
		AuxMaxIdleConns:  config.AuxMaxIdleConns,
		DBConnect:        config.DBConnect,
	})

	// hide the default help command (allow only `--help` flag)
	hb.RootCmd.SetHelpCommand(&cobra.Command{Hidden: true})

	// https://github.com/hanzoai/backendPB/issues/6136
	hb.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: ModerncDepsCheckHookId,
		Func: func(be *core.BootstrapEvent) error {
			if err := be.Next(); err != nil {
				return err
			}

			// run separately to avoid blocking
			app := be.App
			routine.FireAndForget(func() {
				checkModerncDeps(app)
			})

			return nil
		},
	})

	return hb
}

// Start starts the application, aka. registers the default system
// commands (serve, superuser, version) and executes hb.RootCmd.
func (hb *HanzoBase) Start() error {
	// register system commands
	hb.RootCmd.AddCommand(cmd.NewSuperuserCommand(hb))
	hb.RootCmd.AddCommand(cmd.NewServeCommand(hb, !hb.hideStartBanner))

	return hb.Execute()
}

// Execute initializes the application (if not already) and executes
// the hb.RootCmd with graceful shutdown support.
//
// This method differs from hb.Start() by not registering the default
// system commands!
func (hb *HanzoBase) Execute() error {
	if !hb.skipBootstrap() {
		if err := hb.Bootstrap(); err != nil {
			return err
		}
	}

	done := make(chan bool, 1)

	// listen for interrupt signal to gracefully shutdown the application
	go func() {
		sigch := make(chan os.Signal, 1)
		signal.Notify(sigch, os.Interrupt, syscall.SIGTERM)
		<-sigch

		done <- true
	}()

	// execute the root command
	go func() {
		// note: leave to the commands to decide whether to print their error
		hb.RootCmd.Execute()

		done <- true
	}()

	<-done

	// trigger cleanups
	event := new(core.TerminateEvent)
	event.App = hb
	return hb.OnTerminate().Trigger(event, func(e *core.TerminateEvent) error {
		return e.App.ResetBootstrapState()
	})
}

// eagerParseFlags parses the global app flags before calling pb.RootCmd.Execute().
// so we can have all HanzoBase flags ready for use on initialization.
func (hb *HanzoBase) eagerParseFlags(config *Config) error {
	hb.RootCmd.PersistentFlags().StringVar(
		&hb.dataDirFlag,
		"dir",
		config.DefaultDataDir,
		"the HanzoBase data directory",
	)

	hb.RootCmd.PersistentFlags().StringVar(
		&hb.encryptionEnvFlag,
		"encryptionEnv",
		config.DefaultEncryptionEnv,
		"the env variable whose value of 32 characters will be used \nas encryption key for the app settings (default none)",
	)

	hb.RootCmd.PersistentFlags().BoolVar(
		&hb.devFlag,
		"dev",
		config.DefaultDev,
		"enable dev mode, aka. printing logs and sql statements to the console",
	)

	hb.RootCmd.PersistentFlags().IntVar(
		&hb.queryTimeout,
		"queryTimeout",
		int(config.DefaultQueryTimeout.Seconds()),
		"the default SELECT queries timeout in seconds",
	)

	return hb.RootCmd.ParseFlags(os.Args[1:])
}

// skipBootstrap eagerly checks if the app should skip the bootstrap process:
// - already bootstrapped
// - is unknown command
// - is the default help command
// - is the default version command
//
// https://github.com/hanzoissues/404
// https://github.com/hanzodiscussions/1267
func (hb *HanzoBase) skipBootstrap() bool {
	flags := []string{
		"-h",
		"--help",
		"-v",
		"--version",
	}

	if hb.IsBootstrapped() {
		return true // already bootstrapped
	}

	cmd, _, err := hb.RootCmd.Find(os.Args[1:])
	if err != nil {
		return true // unknown command
	}

	for _, arg := range os.Args {
		if !list.ExistInSlice(arg, flags) {
			continue
		}

		// ensure that there is no user defined flag with the same name/shorthand
		trimmed := strings.TrimLeft(arg, "-")
		if len(trimmed) > 1 && cmd.Flags().Lookup(trimmed) == nil {
			return true
		}
		if len(trimmed) == 1 && cmd.Flags().ShorthandLookup(trimmed) == nil {
			return true
		}
	}

	return false
}

// inspectRuntime tries to find the base executable directory and how it was run.
//
// note: we are using os.Args[0] and not os.Executable() since it could
// break existing aliased binaries (eg. the community maintained homebrew package)
func inspectRuntime() (baseDir string, withGoRun bool) {
	if strings.HasPrefix(os.Args[0], os.TempDir()) {
		// probably ran with go run
		withGoRun = true
		baseDir, _ = os.Getwd()
	} else {
		// probably ran with go build
		withGoRun = false
		baseDir = filepath.Dir(os.Args[0])
	}
	return
}

// newErrWriter returns a red colored stderr writter.
func newErrWriter() *coloredWriter {
	return &coloredWriter{
		w: os.Stderr,
		c: color.New(color.FgRed),
	}
}

// coloredWriter is a small wrapper struct to construct a [color.Color] writter.
type coloredWriter struct {
	w io.Writer
	c *color.Color
}

// Write writes the p bytes using the colored writer.
func (colored *coloredWriter) Write(p []byte) (n int, err error) {
	colored.c.SetWriter(colored.w)
	defer colored.c.UnsetWriter(colored.w)

	return colored.c.Print(string(p))
}
