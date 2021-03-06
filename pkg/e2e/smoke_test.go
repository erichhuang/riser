// +build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path"
	"riser/pkg/rc"
	"strings"
	"testing"
	"time"

	"github.com/ghodss/yaml"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/riser-platform/riser-server/api/v1/model"
	"github.com/riser-platform/riser/sdk"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const defaultCommandTimeout = 10 * time.Second

type configMap struct {
	Data map[string]string `json:"data"`
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

type singleStageTestContext struct {
	kubeContext   string
	riserContext  string
	riserStage    string
	ingressIP     string
	ingressDomain string
}

// Initial attempt at e2e testing. Just run through a smoke test of a simple happy path. Lots of refactoring to do as we add more tests.
// Kube and Riser context must be pointing to the correct location
func Test_Smoke(t *testing.T) {
	var testContext *singleStageTestContext
	var httpClient *ingressClient
	var riserClient *sdk.Client

	tmpDir, err := ioutil.TempDir(os.TempDir(), "riser-e2e-")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	step("setup test context", func() {
		testContext = setupSingleStageTestContext(t)
		httpClient = NewIngressClient(testContext.ingressIP)
		var err error
		riserClient, err = getRiserClient()
		require.NoError(t, err)
	})

	appName := fmt.Sprintf("e2e-%s", randomString(6))
	namespace := "apps"
	baseAppUrl := fmt.Sprintf("https://%s.%s.%s", appName, namespace, testContext.ingressDomain)
	appUrl := func(pathAndQuery string) string {
		return fmt.Sprintf("%s/%s", baseAppUrl, pathAndQuery)
	}
	var appId string
	step(fmt.Sprintf("create app %q", appName), func() {
		var err error

		shellOrFail(t, "riser apps new %s", appName)

		apps, err := riserClient.Apps.List()
		require.NoError(t, err)
		// TODO: Add Apps.Get() so that we don't have to do this (although it does add coverage to Apps.List()  :)
		for _, app := range apps {
			if app.Name == appName {
				appId = app.Id
				break
			}
		}
		require.NotEmpty(t, appId)

		appCfg := model.AppConfig{
			Name:  appName,
			Id:    appId,
			Image: "tshak/testdummy",
			Environment: map[string]intstr.IntOrString{
				"env1": intstr.FromString("val1"),
			},
			Expose: &model.AppConfigExpose{
				ContainerPort: 8000,
			},
		}

		appCfgBytes, err := yaml.Marshal(appCfg)
		require.NoError(t, err)
		appCfgPath := path.Join(tmpDir, "app.yaml")
		err = ioutil.WriteFile(appCfgPath, appCfgBytes, 0644)
		require.NoError(t, err)
	})

	versionA := "0.0.15"
	step(fmt.Sprintf("deploy version %q", versionA), func() {
		shellOrFail(t, "cd %s && riser deploy %s %s ", tmpDir, versionA, testContext.riserStage)

		err = httpClient.RetryGet(appUrl("/version"), func(r *httpResult) bool {
			return string(r.body) == versionA
		})
		require.NoError(t, err)

		envResponse, err := httpClient.Get(appUrl("/env"))
		require.NoError(t, err)
		assert.Equal(t, envResponse.StatusCode, http.StatusOK)

		envBody, err := ioutil.ReadAll(envResponse.Body)
		require.NoError(t, err)

		envMap := parseTestDummyEnv(envBody)
		require.Equal(t, "val1", envMap["ENV1"])
	})

	secretName := "secret1"
	secretValue := "secretVal1"
	step("create secret", func() {
		shellOrFail(t, "cd %s && riser secrets save %s %s %s", tmpDir, secretName, secretValue, testContext.riserStage)
		// We do not wait for the secret to be available in k8s. The next deployment should have the secret ref and
		// not become available until the secret is present.
	})

	versionB := "0.0.16"
	step(fmt.Sprintf("deploy version %q", versionB), func() {
		shellOrFail(t, "cd %s && riser deploy %s %s ", tmpDir, versionB, testContext.riserStage)

		err := httpClient.RetryGet(appUrl("/version"), func(r *httpResult) bool {
			return string(r.body) == versionB
		})
		require.NoError(t, err)

		envResponse, err := httpClient.Get(appUrl("/env"))
		require.NoError(t, err)
		assert.Equal(t, envResponse.StatusCode, http.StatusOK)

		envBody, err := ioutil.ReadAll(envResponse.Body)
		require.NoError(t, err)

		envMap := parseTestDummyEnv(envBody)
		require.Equal(t, "val1", envMap["ENV1"])
		require.Equal(t, secretValue, envMap[strings.ToUpper(secretName)])
	})

	step(fmt.Sprintf("delete deployment %q", appName), func() {
		shellOrFail(t, "cd %s && riser deployments delete %s %s --no-prompt", tmpDir, appName, testContext.riserStage)

		// Wait until no deployments in status
		err := Retry(func() (bool, error) {
			appStatus, err := riserClient.Apps.GetStatus(appName)
			if err != nil {
				return true, err
			}

			return len(appStatus.Deployments) == 0, err
		})
		require.NoError(t, err)

		// Check kube resources
		err = Retry(func() (bool, error) {
			configResult := shellOrFail(t, fmt.Sprintf("kubectl get config %s -n %s --ignore-not-found", appName, namespace))
			return configResult == "", nil
		})
		assert.NoError(t, err)

		err = Retry(func() (bool, error) {
			routeResult := shellOrFail(t, fmt.Sprintf("kubectl get route %s -n %s --ignore-not-found", appName, namespace))
			return routeResult == "", nil
		})
		assert.NoError(t, err)
	})
}

func parseTestDummyEnv(envBody []byte) map[string]string {
	envMap := map[string]string{}
	lines := strings.Split(string(envBody), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	return envMap
}

// I had too much friction w/Ginkgo and generally don't like strict BDD. This is trivial and good enough for real time output and timings.
// The big thing missing here is lack of structured output which can be fixed easily if we really need it. For now this is just for
// human consumption.
func step(message string, fn func()) {
	fmt.Printf("• %s", message)
	start := time.Now()
	fn()
	fmt.Printf(" (%dms)\n", time.Since(start).Milliseconds())
}

func setupSingleStageTestContext(t *testing.T) *singleStageTestContext {
	return &singleStageTestContext{
		kubeContext:   shellOrFail(t, "kubectl config current-context"),
		riserContext:  shellOrFail(t, "riser context current"),
		riserStage:    shellOrFail(t, `kubectl get cm riser-controller -n riser-system -o jsonpath="{.data['RISER_STAGE']}"`),
		ingressIP:     shellOrFail(t, "kubectl get service istio-ingressgateway -n istio-system -o jsonpath='{.status.loadBalancer.ingress[0].ip}'"),
		ingressDomain: getRiserDomain(t),
	}
}

func getRiserDomain(t *testing.T) string {
	// We can't use jsonpath because of how knative stores domain config
	domainConfigJson := shellOrFail(t, "kubectl get cm config-domain -n knative-serving -o json")
	domainConfigMap := configMap{}
	err := json.Unmarshal([]byte(domainConfigJson), &domainConfigMap)
	require.NoError(t, err)
	var domain string
	for key := range domainConfigMap.Data {
		domain = key
		break
	}
	require.NotEmpty(t, domain)
	return domain
}

func getRiserClient() (*sdk.Client, error) {
	cfg, err := rc.LoadRc()
	if err != nil {
		return nil, err
	}

	ctx, err := cfg.CurrentContext()
	if err != nil {
		return nil, err
	}

	client, err := sdk.NewClient(ctx.ServerURL, ctx.Apikey)
	if err != nil {
		return nil, err
	}

	if ctx.Secure != nil && !*ctx.Secure {
		client.MakeInsecure()
	}

	return client, err
}

func shellOrFailTimeout(t *testing.T, timeout time.Duration, format string, args ...interface{}) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return shellOrFailContext(t, ctx, format, args...)
}

func shellOrFail(t *testing.T, format string, args ...interface{}) string {
	return shellOrFailTimeout(t, defaultCommandTimeout, format, args...)
}

func shellOrFailContext(t *testing.T, ctx context.Context, format string, args ...interface{}) string {
	output, err := shellContext(ctx, format, args...)
	if err != nil {
		t.Fatalf("Shell command failed: %v", err)
	}

	return output
}

func shellContext(ctx context.Context, format string, args ...interface{}) (string, error) {
	command := fmt.Sprintf(format, args...)
	c := exec.CommandContext(ctx, "sh", "-c", command)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return "", errors.Wrap(err, "error getting stdout pipe")
	}
	c.Stderr = c.Stdout

	var output []byte

	// The exec package is broken when it comes to cancellation. Without this hack a long running process cannot be cancelled.
	// https://github.com/golang/go/issues/23019
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			output = append(output, scanner.Bytes()...)
		}
	}()

	err = c.Run()

	if err != nil {
		if ctx.Err() != nil {
			err = errors.Wrap(ctx.Err(), err.Error())
		}
		return string(output), fmt.Errorf("command %q failed: %q %v", command, string(output), err)
	}
	return string(output), nil
}

const chars = "abcdefghijklmnopqrstuvwxyz0123456789"

func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}
