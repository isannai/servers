package tunnel

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/isannai/isann-servers/pkg/glog"
	"github.com/isannai/isann-servers/pkg/setup"
)

// Base holds shared state used by both Broker and Provider.
type Base struct {
	Cfg          Config
	Auth         AuthConfig
	CfgMu       sync.RWMutex
	Sessions     sync.Map
	CertHash     string
	NodeIdentity setup.NodeIdentity
	StartTime    time.Time
	Log          *glog.Logger
	LogBuf       *glog.RingBuffer

	// Root anchor — directory that contains isann.config.json.
	Root string
	// Parsed isann.config.json. Always non-nil after NewBase (defaults applied).
	Iann *IannConfig
	// Resolved absolute path of the packages directory (= Root + Iann.Paths.Packages,
	// default "packages"). Provider/Broker use this for engine manifest lookup,
	// model storage, dep libraries, etc.
	PackagesDir string
	// Resolved absolute path of the models directory (defaults to PackagesDir/models).
	ModelsDir string
	// Resolved absolute outputs / logs directories.
	OutputsDir string
	LogsDir    string
}

// NewBase creates a new Base with the given config + iann anchor.
// root: directory containing isann.config.json.
// iann: parsed anchor; if nil, an empty default is used (cwd-relative paths).
func NewBase(cfg Config, root string, iann *IannConfig) *Base {
	if iann == nil {
		iann = &IannConfig{Version: "1.0"}
	}
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	packagesDir := ResolvePath(root, iann.Paths.Packages, "packages")
	modelsDir := ResolvePath(root, iann.Paths.Models, filepath.Join(packagesDir, "models"))
	outputsDir := ResolvePath(root, iann.Paths.Outputs, "outputs")
	logsDir := ResolvePath(root, iann.Paths.Logs, "logs")
	identity, err := setup.DeriveNodeIdentity()
	if err != nil {
		log.Fatalf("[backend] failed to derive node identity: %v", err)
	}

	// Load auth.json from same directory as main config
	var authCfg AuthConfig
	if cfg.ConfigFile != "" {
		ac, err := LoadAuthConfig(cfg.ConfigFile)
		if err != nil {
			log.Printf("[backend] auth config load error: %v (using defaults)", err)
			authCfg.Mode = "public"
		} else {
			authCfg = ac
			log.Printf("[backend] auth config loaded: mode=%s owner=%s", ac.Mode, ac.Owner)
		}
	} else {
		authCfg.Mode = "public"
	}

	// If emblem is set but file is missing, clear it in memory AND disk so
	// the UI doesn't request a non-existent file (404). Provider must restart
	// after Upload anyway, so this only affects stale or hand-edited configs.
	if cfg.Emblem != "" && cfg.HomeDir != "" {
		full := filepath.Join(cfg.HomeDir, cfg.Emblem)
		if _, err := os.Stat(full); err != nil {
			log.Printf("[backend] emblem file missing (%s) — clearing", full)
			cfg.Emblem = ""
			if cfg.ConfigFile != "" {
				if saveErr := SaveConfig(cfg); saveErr != nil {
					log.Printf("[backend] emblem cleanup save failed: %v", saveErr)
				}
			}
		}
	}

	// Log destination is hardcoded per mode rather than driven by conf —
	// keeps the per-host conf files free of logging knobs and gives a
	// predictable layout: <root>/logs/<mode>.log (broker.log / provider.log
	// / relay.log), daily rotate, 14 files retained. The conf's `log`
	// block (if present) is silently ignored.
	logFile := "logs/" + string(cfg.Mode) + ".log"
	if root != "" {
		logFile = filepath.Join(root, logFile)
	}
	logCfg := glog.Config{
		Output:   "both",
		File:     logFile,
		Rotate:   "daily",
		MaxFiles: 14,
	}
	logger := glog.New(logCfg)
	b := &Base{
		Cfg:          cfg,
		Auth:         authCfg,
		StartTime:    time.Now(),
		Log:          logger,
		LogBuf:       logger.Buffer(),
		NodeIdentity: identity,
		Root:         root,
		Iann:         iann,
		PackagesDir:  packagesDir,
		ModelsDir:    modelsDir,
		OutputsDir:   outputsDir,
		LogsDir:      logsDir,
	}
	log.Printf("[backend] node identity: %s", identity.Address)
	log.Printf("[backend] root=%s packages=%s models=%s", root, packagesDir, modelsDir)
	return b
}
