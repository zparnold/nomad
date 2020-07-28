package taskrunner

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	agentconsul "github.com/hashicorp/nomad/command/agent/consul"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/pkg/errors"
)

const envoyBootstrapHookName = "envoy_bootstrap"

type consulTransportConfig struct {
	HTTPAddr  string // required
	Auth      string // optional, env CONSUL_HTTP_AUTH
	SSL       string // optional, env CONSUL_HTTP_SSL
	VerifySSL string // optional, env CONSUL_HTTP_SSL_VERIFY
	CAFile    string // optional, arg -ca-file
	CertFile  string // optional, arg -client-cert
	KeyFile   string // optional, arg -client-key
	// CAPath (dir) not supported by Nomad's config object
}

func newConsulTransportConfig(consul *config.ConsulConfig) consulTransportConfig {
	return consulTransportConfig{
		HTTPAddr:  consul.Addr,
		Auth:      consul.Auth,
		SSL:       decodeTriState(consul.EnableSSL),
		VerifySSL: decodeTriState(consul.VerifySSL),
		CAFile:    consul.CAFile,
		CertFile:  consul.CertFile,
		KeyFile:   consul.KeyFile,
	}
}

type envoyBootstrapHookConfig struct {
	consul consulTransportConfig
	alloc  *structs.Allocation
	logger hclog.Logger
}

func decodeTriState(b *bool) string {
	switch {
	case b == nil:
		return ""
	case *b:
		return "true"
	default:
		return "false"
	}
}

func newEnvoyBootstrapHookConfig(alloc *structs.Allocation, consul *config.ConsulConfig, logger hclog.Logger) *envoyBootstrapHookConfig {
	return &envoyBootstrapHookConfig{
		alloc:  alloc,
		logger: logger,
		consul: newConsulTransportConfig(consul),
	}
}

const (
	envoyBaseAdminPort      = 19000
	envoyAdminBindEnvPrefix = "NOMAD_ENVOY_ADMIN_ADDR_"
)

// envoyBootstrapHook writes the bootstrap config for the Connect Envoy proxy
// sidecar.
type envoyBootstrapHook struct {
	// alloc is the allocation with the envoy task being bootstrapped.
	alloc *structs.Allocation

	// Bootstrapping Envoy requires talking directly to Consul to generate
	// the bootstrap.json config. Runtime Envoy configuration is done via
	// Consul's gRPC endpoint. There are many security parameters to configure
	// before contacting Consul.
	consulConfig consulTransportConfig

	// logger is used to log things
	logger hclog.Logger
}

func newEnvoyBootstrapHook(c *envoyBootstrapHookConfig) *envoyBootstrapHook {
	return &envoyBootstrapHook{
		alloc:        c.alloc,
		consulConfig: c.consul,
		logger:       c.logger.Named(envoyBootstrapHookName),
	}
}

func (envoyBootstrapHook) Name() string {
	return envoyBootstrapHookName
}

func (_ *envoyBootstrapHook) extractNameAndKind(kind structs.TaskKind) (string, string, error) {
	serviceKind := kind.Name()
	serviceName := kind.Value()

	switch serviceKind {
	case structs.ConnectProxyPrefix, structs.ConnectIngressPrefix:
	default:
		return "", "", errors.New("envoy must be used as connect sidecar or gateway")
	}

	if serviceName == "" {
		return "", "", errors.New("envoy must be configured with a service name")
	}

	return serviceKind, serviceName, nil
}

func (h *envoyBootstrapHook) lookupService(svcKind, svcName, tgName string) (*structs.Service, error) {
	tg := h.alloc.Job.LookupTaskGroup(h.alloc.TaskGroup)

	var service *structs.Service
	for _, s := range tg.Services {
		if s.Name == svcName {
			service = s
			break
		}
	}

	if service == nil {
		if svcKind == structs.ConnectProxyPrefix {
			return nil, errors.New("connect proxy sidecar task exists but no services configured with a sidecar")
		} else {
			return nil, errors.New("connect gateway task exists but no service associated")
		}
	}

	return service, nil
}

// Prestart creates an envoy bootstrap config file.
//
// Must be aware of both launching envoy as a sidecar proxy, as well as a connect gateway.
func (h *envoyBootstrapHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	fmt.Println("envoyBootstrapHook.Prestart, task:", req.Task.Name)

	if !req.Task.Kind.IsConnectProxy() && !req.Task.Kind.IsAnyConnectGateway() {
		// Not a Connect proxy sidecar
		resp.Done = true
		return nil
	}

	serviceKind, serviceName, err := h.extractNameAndKind(req.Task.Kind)
	if err != nil {
		return err
	}

	service, err := h.lookupService(serviceKind, serviceName, h.alloc.TaskGroup)
	if err != nil {
		return err
	}

	h.logger.Debug("bootstrapping Consul "+serviceKind, "task", req.Task.Name, "service", serviceName)
	fmt.Println("-- bootstrapping kind:", serviceKind, "task:", req.Task.Name, "service:", serviceName)

	// Envoy runs an administrative API on the loopback interface. If multiple sidecars
	// are running, the bind addresses need to have unique ports.
	// TODO: support running in host netns, using freeport to find available port
	envoyAdminBind := buildEnvoyAdminBind(h.alloc, req.Task.Name)
	resp.Env = map[string]string{
		helper.CleanEnvVar(envoyAdminBindEnvPrefix+serviceName, '_'): envoyAdminBind,
	}

	// Envoy bootstrap configuration may contain a Consul token, so write
	// it to the secrets directory like Vault tokens.
	bootstrapFilePath := filepath.Join(req.TaskDir.SecretsDir, "envoy_bootstrap.json")

	siToken, err := h.maybeLoadSIToken(req.Task.Name, req.TaskDir.SecretsDir)
	if err != nil {
		h.logger.Error("failed to generate envoy bootstrap config", "sidecar_for", service.Name)
		return errors.Wrap(err, "failed to generate envoy bootstrap config")
	}
	h.logger.Debug("check for SI token for task", "task", req.Task.Name, "exists", siToken != "")

	bootstrap := h.newEnvoyBootstrapArgs(h.alloc.TaskGroup, service, envoyAdminBind, siToken, bootstrapFilePath)
	bootstrapArgs := bootstrap.args()
	bootstrapEnv := bootstrap.env(os.Environ())

	// Since Consul services are registered asynchronously with this task
	// hook running, retry a small number of times with backoff.
	for tries := 3; ; tries-- {

		cmd := exec.CommandContext(ctx, "consul", bootstrapArgs...)
		cmd.Env = bootstrapEnv

		// Redirect output to secrets/envoy_bootstrap.json
		fd, err := os.Create(bootstrapFilePath)
		if err != nil {
			return fmt.Errorf("error creating secrets/envoy_bootstrap.json for envoy: %v", err)
		}
		cmd.Stdout = fd

		buf := bytes.NewBuffer(nil)
		cmd.Stderr = buf

		// Generate bootstrap
		err = cmd.Run()

		// Close bootstrap.json
		fd.Close()

		if err == nil {
			// Happy path! Bootstrap was created, exit.
			break
		}

		// Check for error from command
		if tries == 0 {
			h.logger.Error("error creating bootstrap configuration for Connect proxy sidecar", "error", err, "stderr", buf.String())

			// Cleanup the bootstrap file. An errors here is not
			// important as (a) we test to ensure the deletion
			// occurs, and (b) the file will either be rewritten on
			// retry or eventually garbage collected if the task
			// fails.
			os.Remove(bootstrapFilePath)

			// ExitErrors are recoverable since they indicate the
			// command was runnable but exited with a unsuccessful
			// error code.
			_, recoverable := err.(*exec.ExitError)
			return structs.NewRecoverableError(
				fmt.Errorf("error creating bootstrap configuration for Connect proxy sidecar: %v", err),
				recoverable,
			)
		}

		// Sleep before retrying to give Consul services time to register
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			// Killed before bootstrap, exit without setting Done
			return nil
		}
	}

	// Bootstrap written. Mark as done and move on.
	resp.Done = true
	return nil
}

func buildEnvoyAdminBind(alloc *structs.Allocation, taskName string) string {
	port := envoyBaseAdminPort
	for idx, task := range alloc.Job.LookupTaskGroup(alloc.TaskGroup).Tasks {
		if task.Name == taskName {
			port += idx
			break
		}
	}
	return fmt.Sprintf("localhost:%d", port)
}

func (h *envoyBootstrapHook) writeConfig(filename, config string) error {
	if err := ioutil.WriteFile(filename, []byte(config), 0440); err != nil {
		_ = os.Remove(filename)
		return err
	}
	return nil
}

func (h *envoyBootstrapHook) execute(cmd *exec.Cmd) (string, error) {
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		_, recoverable := err.(*exec.ExitError)
		// ExitErrors are recoverable since they indicate the
		// command was runnable but exited with a unsuccessful
		// error code.
		return stderr.String(), structs.NewRecoverableError(err, recoverable)
	}
	return stdout.String(), nil
}

func (h *envoyBootstrapHook) newEnvoyBootstrapArgs(tgName string, svc *structs.Service, envoyAdminBind, siToken, filepath string) envoyBootstrapArgs {
	var (
		sidecarForID string // sidecar only
		gateway      string // gateway only
	)

	if svc.Connect.HasSidecar() {
		sidecarForID = agentconsul.MakeAllocServiceID(h.alloc.ID, "group-"+tgName, svc)
	}

	if svc.Connect.IsGateway() {
		gateway = "ingress" // more types in the future
	}

	// todo(shoenig) Should connect directly to Consul if the sidecar is running on the host netns.
	grpcAddr := "unix://" + allocdir.AllocGRPCSocket

	h.logger.Debug("bootstrapping envoy",
		"sidecar_for", svc.Name, "bootstrap_file", filepath,
		"sidecar_for_id", sidecarForID, "grpc_addr", grpcAddr,
		"admin_bind", envoyAdminBind, "gateway", gateway,
	)

	return envoyBootstrapArgs{
		consulConfig:   h.consulConfig,
		sidecarFor:     sidecarForID,
		grpcAddr:       grpcAddr,
		envoyAdminBind: envoyAdminBind,
		siToken:        siToken,
		gateway:        gateway,
	}
}

// envoyBootstrapArgs is used to accumulate CLI arguments that will be passed
// along to the exec invocation of consul which will then generate the bootstrap
// configuration file for envoy.
type envoyBootstrapArgs struct {
	consulConfig   consulTransportConfig
	sidecarFor     string // sidecars only
	grpcAddr       string
	envoyAdminBind string
	siToken        string
	gateway        string // gateways only
}

// args returns the CLI arguments consul needs in the correct order, with the
// -token argument present or not present depending on whether it is set.
func (e envoyBootstrapArgs) args() []string {
	arguments := []string{
		"connect",
		"envoy",
		"-grpc-addr", e.grpcAddr,
		"-http-addr", e.consulConfig.HTTPAddr,
		"-admin-bind", e.envoyAdminBind,
		"-bootstrap",
	}

	if v := e.sidecarFor; v != "" {
		arguments = append(arguments, "-sidecar-for", e.sidecarFor)
	}

	if v := e.gateway; v != "" {
		arguments = append(arguments, "-gateway", e.gateway)
	}

	if v := e.siToken; v != "" {
		arguments = append(arguments, "-token", v)
	}

	if v := e.consulConfig.CAFile; v != "" {
		arguments = append(arguments, "-ca-file", v)
	}

	if v := e.consulConfig.CertFile; v != "" {
		arguments = append(arguments, "-client-cert", v)
	}

	if v := e.consulConfig.KeyFile; v != "" {
		arguments = append(arguments, "-client-key", v)
	}

	return arguments
}

// env creates the context of environment variables to be used when exec-ing
// the consul command for generating the envoy bootstrap config. It is expected
// the value of os.Environ() is passed in to be appended to. Because these are
// appended at the end of what will be passed into Cmd.Env, they will override
// any pre-existing values (i.e. what the Nomad agent was launched with).
// https://golang.org/pkg/os/exec/#Cmd
func (e envoyBootstrapArgs) env(env []string) []string {
	if v := e.consulConfig.Auth; v != "" {
		env = append(env, fmt.Sprintf("%s=%s", "CONSUL_HTTP_AUTH", v))
	}
	if v := e.consulConfig.SSL; v != "" {
		env = append(env, fmt.Sprintf("%s=%s", "CONSUL_HTTP_SSL", v))
	}
	if v := e.consulConfig.VerifySSL; v != "" {
		env = append(env, fmt.Sprintf("%s=%s", "CONSUL_HTTP_SSL_VERIFY", v))
	}
	return env
}

// maybeLoadSIToken reads the SI token saved to disk in the secrets directory
// by the service identities prestart hook. This envoy bootstrap hook blocks
// until the sids hook completes, so if the SI token is required to exist (i.e.
// Consul ACLs are enabled), it will be in place by the time we try to read it.
func (h *envoyBootstrapHook) maybeLoadSIToken(task, dir string) (string, error) {
	tokenPath := filepath.Join(dir, sidsTokenFile)
	token, err := ioutil.ReadFile(tokenPath)
	if err != nil {
		if !os.IsNotExist(err) {
			h.logger.Error("failed to load SI token", "task", task, "error", err)
			return "", errors.Wrapf(err, "failed to load SI token for %s", task)
		}
		h.logger.Trace("no SI token to load", "task", task)
		return "", nil // token file does not exist
	}
	h.logger.Trace("recovered pre-existing SI token", "task", task)
	return string(token), nil
}
