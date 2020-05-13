package taskrunner

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	consulapi "github.com/hashicorp/consul/api"
	consultest "github.com/hashicorp/consul/sdk/testutil"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/client/testutil"
	agentconsul "github.com/hashicorp/nomad/command/agent/consul"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/stretchr/testify/require"
)

func getTestConsul(t *testing.T) *consultest.TestServer {
	testConsul, err := consultest.NewTestServerConfig(func(c *consultest.TestServerConfig) {
		if !testing.Verbose() { // disable consul logging if -v not set
			c.Stdout = ioutil.Discard
			c.Stderr = ioutil.Discard
		}
	})
	require.NoError(t, err, "failed to start test consul server")
	return testConsul
}

func TestConnectNativeHook_Name(t *testing.T) {
	t.Parallel()
	name := new(connectNativeHook).Name()
	require.Equal(t, "connect_native", name)
}

func setupCertDirs(t *testing.T) (string, string) {
	fd, err := ioutil.TempFile("", "connect_native_testcert")
	require.NoError(t, err)
	_, err = fd.WriteString("ABCDEF")
	require.NoError(t, err)
	err = fd.Close()
	require.NoError(t, err)

	d, err := ioutil.TempDir("", "connect_native_testsecrets")
	require.NoError(t, err)
	return fd.Name(), d
}

func cleanupCertDirs(t *testing.T, original, secrets string) {
	err := os.Remove(original)
	require.NoError(t, err)
	err = os.RemoveAll(secrets)
	require.NoError(t, err)
}

func TestConnectNativeHook_copyCertificate(t *testing.T) {
	t.Parallel()

	f, d := setupCertDirs(t)
	defer cleanupCertDirs(t, f, d)

	t.Run("no source", func(t *testing.T) {
		err := new(connectNativeHook).copyCertificate("", d, "out.pem")
		require.NoError(t, err)
	})

	t.Run("normal", func(t *testing.T) {
		err := new(connectNativeHook).copyCertificate(f, d, "out.pem")
		require.NoError(t, err)
		b, err := ioutil.ReadFile(filepath.Join(d, "out.pem"))
		require.NoError(t, err)
		require.Equal(t, "ABCDEF", string(b))
	})
}

func TestConnectNativeHook_copyCertificates(t *testing.T) {
	t.Parallel()

	f, d := setupCertDirs(t)
	defer cleanupCertDirs(t, f, d)

	t.Run("normal", func(t *testing.T) {
		err := new(connectNativeHook).copyCertificates(consulTransportConfig{
			CAFile:   f,
			CertFile: f,
			KeyFile:  f,
		}, d)
		require.NoError(t, err)
		ls, err := ioutil.ReadDir(d)
		require.NoError(t, err)
		require.Equal(t, 3, len(ls))
	})

	t.Run("no source", func(t *testing.T) {
		err := new(connectNativeHook).copyCertificates(consulTransportConfig{
			CAFile:   "/does/not/exist.pem",
			CertFile: "/does/not/exist.pem",
			KeyFile:  "/does/not/exist.pem",
		}, d)
		require.EqualError(t, err, "failed to open consul TLS certificate: open /does/not/exist.pem: no such file or directory")
	})
}

func TestConnectNativeHook_tlsEnv(t *testing.T) {
	t.Parallel()

	// the hook config comes from client config
	emptyHook := new(connectNativeHook)
	fullHook := &connectNativeHook{
		consulConfig: consulTransportConfig{
			Auth:      "user:password",
			SSL:       "true",
			VerifySSL: "true",
			CAFile:    "/not/real/ca.pem",
			CertFile:  "/not/real/cert.pem",
			KeyFile:   "/not/real/key.pem",
		},
	}

	// existing config from task env stanza
	taskEnv := map[string]string{
		"CONSUL_CACERT":          "fakeCA.pem",
		"CONSUL_CLIENT_CERT":     "fakeCert.pem",
		"CONSUL_CLIENT_KEY":      "fakeKey.pem",
		"CONSUL_HTTP_AUTH":       "foo:bar",
		"CONSUL_HTTP_SSL":        "false",
		"CONSUL_HTTP_SSL_VERIFY": "false",
	}

	t.Run("empty hook and empty task", func(t *testing.T) {
		result := emptyHook.tlsEnv(nil)
		require.Empty(t, result)
	})

	t.Run("empty hook and non-empty task", func(t *testing.T) {
		result := emptyHook.tlsEnv(taskEnv)
		require.Empty(t, result) // tlsEnv only overrides; task env is actually set elsewhere
	})

	t.Run("non-empty hook and empty task", func(t *testing.T) {
		result := fullHook.tlsEnv(nil)
		require.Equal(t, map[string]string{
			// ca files are specifically copied into FS namespace
			"CONSUL_CACERT":          "/secrets/consul_ca_file",
			"CONSUL_CLIENT_CERT":     "/secrets/consul_cert_file",
			"CONSUL_CLIENT_KEY":      "/secrets/consul_key_file",
			"CONSUL_HTTP_AUTH":       "user:password",
			"CONSUL_HTTP_SSL":        "true",
			"CONSUL_HTTP_SSL_VERIFY": "true",
		}, result)
	})

	t.Run("non-empty hook and non-empty task", func(t *testing.T) {
		result := fullHook.tlsEnv(taskEnv) // task env takes precedence, nothing gets set here
		require.Empty(t, result)
	})
}

func TestTaskRunner_ConnectNativeHook_Noop(t *testing.T) {
	t.Parallel()
	logger := testlog.HCLogger(t)

	allocDir, cleanup := allocdir.TestAllocDir(t, logger, "ConnectNative")
	defer cleanup()

	alloc := mock.Alloc()
	task := alloc.Job.LookupTaskGroup(alloc.TaskGroup).Tasks[0]

	// run the connect native hook. use invalid consul address as it should not get hit
	h := newConnectNativeHook(newConnectNativeHookConfig(alloc, &config.ConsulConfig{
		Addr: "http://127.0.0.2:1",
	}, logger))

	request := &interfaces.TaskPrestartRequest{
		Task:    task,
		TaskDir: allocDir.NewTaskDir(task.Name),
	}
	require.NoError(t, request.TaskDir.Build(false, nil))

	response := new(interfaces.TaskPrestartResponse)

	// Run the hook
	require.NoError(t, h.Prestart(context.Background(), request, response))

	// Assert the hook is Done
	require.True(t, response.Done)

	// Assert secrets dir is empty (no TLS config set)
	ls, err := ioutil.ReadDir(filepath.Join(request.TaskDir.SecretsDir))
	require.NoError(t, err)
	require.Equal(t, 0, len(ls))
}

func TestTaskRunner_ConnectNativeHook_Ok(t *testing.T) {
	t.Parallel()
	testutil.RequireConsul(t)

	testConsul := getTestConsul(t)
	defer testConsul.Stop()

	alloc := mock.Alloc()
	alloc.AllocatedResources.Shared.Networks = []*structs.NetworkResource{{Mode: "host", IP: "1.1.1.1"}}
	tg := alloc.Job.TaskGroups[0]
	tg.Services = []*structs.Service{{
		Name: "cn-service",
		Connect: &structs.ConsulConnect{
			Native: tg.Tasks[0].Name,
		}},
	}
	tg.Tasks[0].Kind = structs.NewTaskKind("connect-native", "cn-service")

	logger := testlog.HCLogger(t)

	allocDir, cleanup := allocdir.TestAllocDir(t, logger, "ConnectNative")
	defer cleanup()

	// register group services
	consulConfig := consulapi.DefaultConfig()
	consulConfig.Address = testConsul.HTTPAddr
	consulAPIClient, err := consulapi.NewClient(consulConfig)
	require.NoError(t, err)

	consulClient := agentconsul.NewServiceClient(consulAPIClient.Agent(), logger, true)
	go consulClient.Run()
	defer consulClient.Shutdown()
	require.NoError(t, consulClient.RegisterWorkload(agentconsul.BuildAllocServices(mock.Node(), alloc, agentconsul.NoopRestarter())))

	// Run Connect Native hook
	h := newConnectNativeHook(newConnectNativeHookConfig(alloc, &config.ConsulConfig{
		Addr: consulConfig.Address,
	}, logger))
	request := &interfaces.TaskPrestartRequest{
		Task:    tg.Tasks[0],
		TaskDir: allocDir.NewTaskDir(tg.Tasks[0].Name),
	}
	require.NoError(t, request.TaskDir.Build(false, nil))

	response := new(interfaces.TaskPrestartResponse)

	// Run the Connect Native hook
	require.NoError(t, h.Prestart(context.Background(), request, response))

	// Assert the hook is Done
	require.True(t, response.Done)

	// Assert no environment variables configured to be set
	require.Empty(t, response.Env)

	// Assert no secrets were written
	ls, err := ioutil.ReadDir(request.TaskDir.SecretsDir)
	require.NoError(t, err)
	require.Equal(t, 0, len(ls))
}

func TestTaskRunner_ConnectNativeHook_with_SI_token(t *testing.T) {
	t.Parallel()
	testutil.RequireConsul(t)

	testConsul := getTestConsul(t)
	defer testConsul.Stop()

	alloc := mock.Alloc()
	alloc.AllocatedResources.Shared.Networks = []*structs.NetworkResource{{Mode: "host", IP: "1.1.1.1"}}
	tg := alloc.Job.TaskGroups[0]
	tg.Services = []*structs.Service{{
		Name: "cn-service",
		Connect: &structs.ConsulConnect{
			Native: tg.Tasks[0].Name,
		}},
	}
	tg.Tasks[0].Kind = structs.NewTaskKind("connect-native", "cn-service")

	logger := testlog.HCLogger(t)

	allocDir, cleanup := allocdir.TestAllocDir(t, logger, "ConnectNative")
	defer cleanup()

	// register group services
	consulConfig := consulapi.DefaultConfig()
	consulConfig.Address = testConsul.HTTPAddr
	consulAPIClient, err := consulapi.NewClient(consulConfig)
	require.NoError(t, err)

	consulClient := agentconsul.NewServiceClient(consulAPIClient.Agent(), logger, true)
	go consulClient.Run()
	defer consulClient.Shutdown()
	require.NoError(t, consulClient.RegisterWorkload(agentconsul.BuildAllocServices(mock.Node(), alloc, agentconsul.NoopRestarter())))

	// Run Connect Native hook
	h := newConnectNativeHook(newConnectNativeHookConfig(alloc, &config.ConsulConfig{
		Addr: consulConfig.Address,
	}, logger))
	request := &interfaces.TaskPrestartRequest{
		Task:    tg.Tasks[0],
		TaskDir: allocDir.NewTaskDir(tg.Tasks[0].Name),
	}
	require.NoError(t, request.TaskDir.Build(false, nil))

	// Insert service identity token in the secrets directory
	token := uuid.Generate()
	siTokenFile := filepath.Join(request.TaskDir.SecretsDir, sidsTokenFile)
	err = ioutil.WriteFile(siTokenFile, []byte(token), 0440)
	require.NoError(t, err)

	response := new(interfaces.TaskPrestartResponse)
	response.Env = make(map[string]string)

	// Run the Connect Native hook
	require.NoError(t, h.Prestart(context.Background(), request, response))

	// Assert the hook is Done
	require.True(t, response.Done)

	// Assert environment variable for token is set
	require.NotEmpty(t, response.Env)
	require.Equal(t, token, response.Env["CONSUL_HTTP_TOKEN"])

	// Assert no additional secrets were written
	ls, err := ioutil.ReadDir(request.TaskDir.SecretsDir)
	require.NoError(t, err)
	require.Equal(t, 1, len(ls))
}

func TestTaskRunner_ConnectNativeHook_shareTLS(t *testing.T) {
	t.Parallel()
	testutil.RequireConsul(t)

	fakeCert, fakeCertDir := setupCertDirs(t)
	defer cleanupCertDirs(t, fakeCert, fakeCertDir)

	testConsul := getTestConsul(t)
	defer testConsul.Stop()

	alloc := mock.Alloc()
	alloc.AllocatedResources.Shared.Networks = []*structs.NetworkResource{{Mode: "host", IP: "1.1.1.1"}}
	tg := alloc.Job.TaskGroups[0]
	tg.Services = []*structs.Service{{
		Name: "cn-service",
		Connect: &structs.ConsulConnect{
			Native: tg.Tasks[0].Name,
		}},
	}
	tg.Tasks[0].Kind = structs.NewTaskKind("connect-native", "cn-service")

	logger := testlog.HCLogger(t)

	allocDir, cleanup := allocdir.TestAllocDir(t, logger, "ConnectNative")
	defer cleanup()

	// register group services
	consulConfig := consulapi.DefaultConfig()
	consulConfig.Address = testConsul.HTTPAddr
	consulAPIClient, err := consulapi.NewClient(consulConfig)
	require.NoError(t, err)

	consulClient := agentconsul.NewServiceClient(consulAPIClient.Agent(), logger, true)
	go consulClient.Run()
	defer consulClient.Shutdown()
	require.NoError(t, consulClient.RegisterWorkload(agentconsul.BuildAllocServices(mock.Node(), alloc, agentconsul.NoopRestarter())))

	// Run Connect Native hook
	h := newConnectNativeHook(newConnectNativeHookConfig(alloc, &config.ConsulConfig{
		Addr: consulConfig.Address,

		// TLS config consumed by native application
		ShareSSL:  helper.BoolToPtr(true),
		EnableSSL: helper.BoolToPtr(true),
		VerifySSL: helper.BoolToPtr(true),
		CAFile:    fakeCert,
		CertFile:  fakeCert,
		KeyFile:   fakeCert,
		Auth:      "user:password",
	}, logger))
	request := &interfaces.TaskPrestartRequest{
		Task:    tg.Tasks[0],
		TaskDir: allocDir.NewTaskDir(tg.Tasks[0].Name),
		TaskEnv: taskenv.NewEmptyTaskEnv(), // nothing set in env stanza
	}
	require.NoError(t, request.TaskDir.Build(false, nil))

	response := new(interfaces.TaskPrestartResponse)
	response.Env = make(map[string]string)

	// Run the Connect Native hook
	require.NoError(t, h.Prestart(context.Background(), request, response))

	// Assert the hook is Done
	require.True(t, response.Done)

	// Assert environment variable for token is set
	require.NotEmpty(t, response.Env)
	require.Equal(t, map[string]string{
		"CONSUL_CACERT":          "/secrets/consul_ca_file",
		"CONSUL_CLIENT_CERT":     "/secrets/consul_cert_file",
		"CONSUL_CLIENT_KEY":      "/secrets/consul_key_file",
		"CONSUL_HTTP_AUTH":       "user:password",
		"CONSUL_HTTP_SSL":        "true",
		"CONSUL_HTTP_SSL_VERIFY": "true",
	}, response.Env)

	// Assert 3 pem files were written
	ls, err := ioutil.ReadDir(request.TaskDir.SecretsDir)
	require.NoError(t, err)
	require.Equal(t, 3, len(ls))
}

func TestTaskRunner_ConnectNativeHook_shareTLS_override(t *testing.T) {
	t.Parallel()
	testutil.RequireConsul(t)

	fakeCert, fakeCertDir := setupCertDirs(t)
	defer cleanupCertDirs(t, fakeCert, fakeCertDir)

	testConsul := getTestConsul(t)
	defer testConsul.Stop()

	alloc := mock.Alloc()
	alloc.AllocatedResources.Shared.Networks = []*structs.NetworkResource{{Mode: "host", IP: "1.1.1.1"}}
	tg := alloc.Job.TaskGroups[0]
	tg.Services = []*structs.Service{{
		Name: "cn-service",
		Connect: &structs.ConsulConnect{
			Native: tg.Tasks[0].Name,
		}},
	}
	tg.Tasks[0].Kind = structs.NewTaskKind("connect-native", "cn-service")

	logger := testlog.HCLogger(t)

	allocDir, cleanup := allocdir.TestAllocDir(t, logger, "ConnectNative")
	defer cleanup()

	// register group services
	consulConfig := consulapi.DefaultConfig()
	consulConfig.Address = testConsul.HTTPAddr
	consulAPIClient, err := consulapi.NewClient(consulConfig)
	require.NoError(t, err)

	consulClient := agentconsul.NewServiceClient(consulAPIClient.Agent(), logger, true)
	go consulClient.Run()
	defer consulClient.Shutdown()
	require.NoError(t, consulClient.RegisterWorkload(agentconsul.BuildAllocServices(mock.Node(), alloc, agentconsul.NoopRestarter())))

	// Run Connect Native hook
	h := newConnectNativeHook(newConnectNativeHookConfig(alloc, &config.ConsulConfig{
		Addr: consulConfig.Address,

		// TLS config consumed by native application
		ShareSSL:  helper.BoolToPtr(true),
		EnableSSL: helper.BoolToPtr(true),
		VerifySSL: helper.BoolToPtr(true),
		CAFile:    fakeCert,
		CertFile:  fakeCert,
		KeyFile:   fakeCert,
		Auth:      "user:password",
	}, logger))

	taskEnv := taskenv.NewEmptyTaskEnv()
	taskEnv.EnvMap = map[string]string{
		"CONSUL_CACERT":          "/foo/ca.pem",
		"CONSUL_CLIENT_CERT":     "/foo/cert.pem",
		"CONSUL_CLIENT_KEY":      "/foo/key.pem",
		"CONSUL_HTTP_AUTH":       "foo:bar",
		"CONSUL_HTTP_SSL_VERIFY": "false",
		// CONSUL_HTTP_SSL (check the default value is assumed from client config)
	}

	request := &interfaces.TaskPrestartRequest{
		Task:    tg.Tasks[0],
		TaskDir: allocDir.NewTaskDir(tg.Tasks[0].Name),
		TaskEnv: taskEnv, // env stanza is configured w/ non-default tls configs
	}
	require.NoError(t, request.TaskDir.Build(false, nil))

	response := new(interfaces.TaskPrestartResponse)
	response.Env = make(map[string]string)

	// Run the Connect Native hook
	require.NoError(t, h.Prestart(context.Background(), request, response))

	// Assert the hook is Done
	require.True(t, response.Done)

	// Assert environment variable for CONSUL_HTTP_SSL is set, because it was
	// the only one not overridden by task env stanza config
	require.NotEmpty(t, response.Env)
	require.Equal(t, map[string]string{
		"CONSUL_HTTP_SSL": "true",
	}, response.Env)

	// Assert 3 pem files were written (even though they will be ignored)
	// as this is gated by share_tls, not the presense of ca environment variables.
	ls, err := ioutil.ReadDir(request.TaskDir.SecretsDir)
	require.NoError(t, err)
	require.Equal(t, 3, len(ls))
}
