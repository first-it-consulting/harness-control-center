package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"syscall"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/budget"
	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/egress"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/firecracker"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
	"github.com/first-it-consulting/harness-control-center/internal/version"
)

func cmdRunner(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "hcc runner: expected serve or stdio\n\n%s", usage)
		return exitUsage
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr)
	case "stdio":
		return cmdStdio(args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hcc runner: unknown command %q\n", args[0])
		return exitUsage
	}
}

func cmdServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("runner serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "/etc/hcc/runner.yaml", "runner configuration file")
	if _, err := parseFlags(fs, args); err != nil {
		return exitUsage
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := serve(*path, log); err != nil {
		log.Error("runner stopped", "err", err)
		return exitInvalid
	}
	return exitOK
}

func serve(configPath string, log *slog.Logger) error {
	// The runner itself never needs root; only the jailer does (via the
	// sandbox Host). Refusing root keeps that boundary real.
	if os.Geteuid() == 0 {
		return errors.New("refusing to run as root: run the runner as an unprivileged user")
	}
	cfg, err := runnerd.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if me, err := user.Current(); err != nil || me.Username != cfg.RunnerUser {
		return fmt.Errorf("must run as the configured runnerUser %q", cfg.RunnerUser)
	}
	if err := cfg.CheckFiles(); err != nil {
		return err
	}
	// The jail tree (chrootBase) is root's; only these are the runner's.
	for _, d := range []string{cfg.StateDir, cfg.Firecracker.StagingDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	provider, err := firecracker.New(firecracker.Config{
		StagingBase: cfg.Firecracker.StagingDir,
		// One UID/GID per concurrently running VM: uid..uid+maxConcurrent-1.
		Identities: cfg.MaxConcurrent,
		Host:       firecracker.NewHelperHost(cfg.Firecracker.Helper),
	})
	if err != nil {
		return err
	}
	images := make([]string, 0, len(cfg.Images))
	for name := range cfg.Images {
		images = append(images, name)
	}
	netCfg, err := cfg.NetworkConfig()
	if err != nil {
		return err
	}
	store := runnerd.NewFileStore(cfg.StateDir, log)
	rcfg := runner.Config{
		Provider:      provider,
		Target:        cfg.Target,
		Images:        images,
		MaxConcurrent: cfg.MaxConcurrent,
		Console:       runnerd.ConsoleFunc(cfg.StateDir, log),
		Pricing:       cfg.Pricing,
		Results:       runnerd.ResultsFunc(cfg.StateDir),
		MaxDiskMiB:    cfg.MaxDiskMiB,
		Store:         store,
		Recover: func(ctx context.Context, ids []string) error {
			return provider.Recover(ctx, ids, netCfg != nil)
		},
		Log:                 log,
		ScheduleStore:       store,
		MinScheduleInterval: cfg.ScheduleInterval(),
	}
	if netCfg != nil {
		rcfg.Egress = egressFactory(netCfg, cfg.Credentials, log)
	}
	r, err := runner.New(rcfg)
	if err != nil {
		return err
	}

	// Every state change goes to the log, for journald and auditing.
	events, stop := r.Subscribe(1024)
	defer stop()
	go func() {
		for e := range events {
			log.Info("job", "job", e.JobID, "harness", e.Harness, "from", e.From, "to", e.To, "message", e.Message)
		}
	}()

	l, err := runnerd.Listen(cfg.Socket, cfg.SocketGroup)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go pruneJobs(ctx, r, store, cfg.Retention, log)
	log.Info("runner started", "version", version.Version, "target", cfg.Target, "socket", cfg.Socket,
		"maxConcurrent", cfg.MaxConcurrent, "network", netCfg != nil)
	err = runnerd.NewServer(r, cfg.StateDir, log).Serve(ctx, l)
	log.Info("runner stopped; all jobs were stopped")
	return err
}

// pruneJobs enforces the retention limits now and then every hour.
func pruneJobs(ctx context.Context, r *runner.Runner, store *runnerd.FileStore, ret runnerd.Retention, log *slog.Logger) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		jobs := r.Prune(ret.MaxJobs, ret.Age())
		orphans := store.PruneOrphans(ret.Age(), time.Now())
		if len(jobs)+len(orphans) > 0 {
			log.Info("pruned old jobs", "jobs", len(jobs), "orphans", len(orphans))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// egressFactory builds each networked job's proxy from that job's own
// harness. The sandboxes' subnet is never a valid destination. Granted
// credentials are loaded fresh for every job and get a CA of their own.
// The model endpoint is always brokered, so the job's budget can be
// metered and enforced there.
func egressFactory(netCfg *firecracker.NetworkConfig, credsPath string, log *slog.Logger) func(*spec.Harness, string, *budget.Meter) (sandbox.Proxy, error) {
	return func(h *spec.Harness, jobID string, meter *budget.Meter) (sandbox.Proxy, error) {
		engine, err := policy.New(h)
		if err != nil {
			return nil, err
		}
		p := &egress.Proxy{
			Policy:       engine,
			AllowPrivate: h.Spec.Permissions.Network.AllowPrivateNetworks,
			Blocked:      []netip.Prefix{netCfg.Subnet},
			JobID:        jobID,
			Log:          log,
			Meter:        meter,
		}
		byHost := map[string]credentials.Credential{}
		if len(h.Spec.Permissions.Credentials) > 0 {
			if credsPath == "" {
				return nil, errors.New("this runner has no credentials configured")
			}
			store, err := credentials.Load(credsPath)
			if err != nil {
				return nil, err
			}
			if byHost, err = credentials.ForHarness(store, h, func(host string, port int) bool {
				return engine.CheckEgress(host, port).Allowed
			}); err != nil {
				return nil, err
			}
		}
		if m := h.Spec.Model; m != nil {
			if err := brokerModel(byHost, m); err != nil {
				return nil, err
			}
			_, p.ModelHost, p.ModelPort = m.EndpointHostPort()
			if p.ModelHost == "" || p.ModelPort < 1 || p.ModelPort > 65535 {
				return nil, fmt.Errorf("model endpoint %q has no host and port", m.Endpoint)
			}
		}
		var hosts, tlsHosts []string
		for host, c := range byHost {
			hosts = append(hosts, host)
			if c.Scheme == "https" {
				tlsHosts = append(tlsHosts, host)
			}
		}
		if len(tlsHosts) > 0 { // plain-http hosts need no CA
			if p.CA, err = egress.NewCA(tlsHosts, time.Now()); err != nil {
				return nil, err
			}
		}
		p.Credentials = byHost
		log.Info("egress prepared", "job", jobID, "credentials", h.Spec.Permissions.Credentials, "brokered", hosts)
		return p, nil
	}
}

// brokerModel makes sure the model endpoint is brokered: with its
// credential if it names one (which must be for exactly that endpoint),
// otherwise without any (metering only). A plain-http endpoint always
// needs a credential: only the operator can allow plain HTTP.
func brokerModel(byHost map[string]credentials.Credential, m *spec.Model) error {
	scheme, host, port := m.EndpointHostPort()
	c, ok := byHost[host]
	switch {
	case ok && (c.Scheme != scheme || c.Port != port):
		return fmt.Errorf("model endpoint %s://%s:%d does not match its host's credential %q (%s://%s:%d)", scheme, host, port, c.Name, c.Scheme, host, c.Port)
	case ok && m.Credential != "" && c.Name != m.Credential:
		return fmt.Errorf("model endpoint %s is covered by credential %q, not %q", host, c.Name, m.Credential)
	case ok:
		return nil
	case m.Credential != "":
		return fmt.Errorf("credential %q is not for the model endpoint %s", m.Credential, host)
	case scheme == "http":
		return fmt.Errorf("plain-http model endpoint %s needs a credential configured with scheme http on the runner", host)
	}
	byHost[host] = credentials.Credential{Name: "model", Hosts: []string{host}, Scheme: scheme, Port: port}
	return nil
}

func cmdStdio(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runner stdio", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", runnerd.DefaultSocket, "runner socket")
	if _, err := parseFlags(fs, args); err != nil {
		return exitUsage
	}
	if err := runnerd.Relay(context.Background(), *socket, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "hcc runner stdio:", err)
		return exitInvalid
	}
	return exitOK
}
