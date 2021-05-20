package command

import (
	"context"
	"strings"
	"sync"

	"github.com/hashicorp/consul/api"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/internalshared/listenerutil"
	"github.com/hashicorp/vault/internalshared/reloadutil"
	physconsul "github.com/hashicorp/vault/physical/consul"
	"github.com/hashicorp/vault/sdk/version"
	srconsul "github.com/hashicorp/vault/serviceregistration/consul"
	"github.com/hashicorp/vault/vault/diagnose"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

const OperatorDiagnoseEnableEnv = "VAULT_DIAGNOSE"

var (
	_ cli.Command             = (*OperatorDiagnoseCommand)(nil)
	_ cli.CommandAutocomplete = (*OperatorDiagnoseCommand)(nil)
)

type OperatorDiagnoseCommand struct {
	*BaseCommand
	diagnose *diagnose.Session

	flagDebug    bool
	flagSkips    []string
	flagConfigs  []string
	cleanupGuard sync.Once

	reloadFuncsLock *sync.RWMutex
	reloadFuncs     *map[string][]reloadutil.ReloadFunc
	startedCh       chan struct{} // for tests
	reloadedCh      chan struct{} // for tests
	skipEndEnd      bool          // for tests
}

func (c *OperatorDiagnoseCommand) Synopsis() string {
	return "Troubleshoot problems starting Vault"
}

func (c *OperatorDiagnoseCommand) Help() string {
	helpText := `
Usage: vault operator diagnose 

  This command troubleshoots Vault startup issues, such as TLS configuration or
  auto-unseal. It should be run using the same environment variables and configuration
  files as the "vault server" command, so that startup problems can be accurately
  reproduced.

  Start diagnose with a configuration file:
    
     $ vault operator diagnose -config=/etc/vault/config.hcl

  Perform a diagnostic check while Vault is still running:

     $ vault operator diagnose -config=/etc/vault/config.hcl -skip=listener

` + c.Flags().Help()
	return strings.TrimSpace(helpText)
}

func (c *OperatorDiagnoseCommand) Flags() *FlagSets {
	set := NewFlagSets(c.UI)
	f := set.NewFlagSet("Command Options")

	f.StringSliceVar(&StringSliceVar{
		Name:   "config",
		Target: &c.flagConfigs,
		Completion: complete.PredictOr(
			complete.PredictFiles("*.hcl"),
			complete.PredictFiles("*.json"),
			complete.PredictDirs("*"),
		),
		Usage: "Path to a Vault configuration file or directory of configuration " +
			"files. This flag can be specified multiple times to load multiple " +
			"configurations. If the path is a directory, all files which end in " +
			".hcl or .json are loaded.",
	})

	f.StringSliceVar(&StringSliceVar{
		Name:   "skip",
		Target: &c.flagSkips,
		Usage:  "Skip the health checks named as arguments. May be 'listener', 'storage', or 'autounseal'.",
	})

	f.BoolVar(&BoolVar{
		Name:    "debug",
		Target:  &c.flagDebug,
		Default: false,
		Usage:   "Dump all information collected by Diagnose.",
	})
	return set
}

func (c *OperatorDiagnoseCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OperatorDiagnoseCommand) AutocompleteFlags() complete.Flags {
	return c.Flags().Completions()
}

const (
	status_unknown = "[      ] "
	status_ok      = "\u001b[32m[  ok  ]\u001b[0m "
	status_failed  = "\u001b[31m[failed]\u001b[0m "
	status_warn    = "\u001b[33m[ warn ]\u001b[0m "
	same_line      = "\u001b[F"
)

func (c *OperatorDiagnoseCommand) Run(args []string) int {
	f := c.Flags()
	if err := f.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}
	return c.RunWithParsedFlags()
}

func (c *OperatorDiagnoseCommand) RunWithParsedFlags() int {

	if len(c.flagConfigs) == 0 {
		c.UI.Error("Must specify a configuration file using -config.")
		return 1
	}

	c.UI.Output(version.GetVersion().FullVersionNumber(true))
	ctx := diagnose.Context(context.Background(), c.diagnose)
	err := c.offlineDiagnostics(ctx)
	c.diagnose.SetSkipList(c.flagSkips)

	if err != nil {
		return 1
	}
	return 0
}

func (c *OperatorDiagnoseCommand) offlineDiagnostics(ctx context.Context) error {
	rloadFuncs := make(map[string][]reloadutil.ReloadFunc)
	server := &ServerCommand{
		// TODO: set up a different one?
		// In particular, a UI instance that won't output?
		BaseCommand: c.BaseCommand,

		// TODO: refactor to a common place?
		AuditBackends:        auditBackends,
		CredentialBackends:   credentialBackends,
		LogicalBackends:      logicalBackends,
		PhysicalBackends:     physicalBackends,
		ServiceRegistrations: serviceRegistrations,

		// TODO: other ServerCommand options?

		logger:          log.NewInterceptLogger(nil),
		allLoggers:      []log.Logger{},
		reloadFuncs:     &rloadFuncs,
		reloadFuncsLock: new(sync.RWMutex),
	}

	ctx, span := diagnose.StartSpan(ctx, "initialization")
	defer span.End()

	server.flagConfigs = c.flagConfigs
	config, err := server.parseConfig()
	if err != nil {
		return diagnose.SpotError(ctx, "parse-config", err)
	} else {
		diagnose.SpotOk(ctx, "parse-config", "")
	}
	// Check Listener Information
	// TODO: Run Diagnose checks on the actual net.Listeners

	if err := diagnose.Test(ctx, "init-listeners", func(ctx context.Context) error {
		disableClustering := config.HAStorage.DisableClustering
		infoKeys := make([]string, 0, 10)
		info := make(map[string]string)
		status, lns, _, errMsg := server.InitListeners(config, disableClustering, &infoKeys, &info)

		if status != 0 {
			return errMsg
		}

		// Make sure we close all listeners from this point on
		listenerCloseFunc := func() {
			for _, ln := range lns {
				ln.Listener.Close()
			}
		}

		defer c.cleanupGuard.Do(listenerCloseFunc)

		sanitizedListeners := make([]listenerutil.Listener, 0, len(config.Listeners))
		for _, ln := range lns {
			if ln.Config.TLSDisable {
				diagnose.Warn(ctx, "TLS is disabled in a Listener config stanza.")
				continue
			}
			if ln.Config.TLSDisableClientCerts {
				diagnose.Warn(ctx, "TLS for a listener is turned on without requiring client certs.")
			}

			// Check ciphersuite and load ca/cert/key files
			// TODO: TLSConfig returns a reloadFunc and a TLSConfig. We can use this to
			// perform an active probe.
			_, _, err := listenerutil.TLSConfig(ln.Config, make(map[string]string), c.UI)
			if err != nil {
				return err
			}

			sanitizedListeners = append(sanitizedListeners, listenerutil.Listener{
				Listener: ln.Listener,
				Config:   ln.Config,
			})
		}
		return diagnose.ListenerChecks(sanitizedListeners)
	}); err != nil {
		return err
	}

	// Errors in these items could stop Vault from starting but are not yet covered:
	// TODO: logging configuration
	// TODO: SetupTelemetry
	if err := diagnose.Test(ctx, "storage", func(ctx context.Context) error {
		b, err := server.setupStorage(config)
		if err != nil {
			return err
		}

		dirAccess := diagnose.ConsulDirectAccess(config.HAStorage.Config)
		if dirAccess != "" {
			diagnose.Warn(ctx, dirAccess)
		}

		if config.Storage != nil && config.Storage.Type == storageTypeConsul {
			err = physconsul.SetupSecureTLS(api.DefaultConfig(), config.Storage.Config, server.logger, true)
			if err != nil {
				return err
			}

			dirAccess := diagnose.ConsulDirectAccess(config.Storage.Config)
			if dirAccess != "" {
				diagnose.Warn(ctx, dirAccess)
			}
		}

		if config.HAStorage != nil && config.HAStorage.Type == storageTypeConsul {
			err = physconsul.SetupSecureTLS(api.DefaultConfig(), config.HAStorage.Config, server.logger, true)
			if err != nil {
				return err
			}
		}

		// Attempt to use storage backend
		if !c.skipEndEnd {
			err = diagnose.StorageEndToEndLatencyCheck(ctx, b)
			if err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		return err
	}

	return diagnose.Test(ctx, "service-discovery", func(ctx context.Context) error {
		srConfig := config.ServiceRegistration.Config
		// Initialize the Service Discovery, if there is one
		if config.ServiceRegistration != nil && config.ServiceRegistration.Type == "consul" {
			// setupStorage populates the srConfig, so no nil checks are necessary.
			dirAccess := diagnose.ConsulDirectAccess(config.ServiceRegistration.Config)
			if dirAccess != "" {
				diagnose.Warn(ctx, dirAccess)
			}

			// SetupSecureTLS for service discovery uses the same cert and key to set up physical
			// storage. See the consul package in physical for details.
			err = srconsul.SetupSecureTLS(api.DefaultConfig(), srConfig, server.logger, true)
			if err != nil {
				return err
			}
		}
		return nil
	})
}
