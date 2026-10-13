package runtime

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	hcplugin "github.com/hashicorp/go-plugin"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

func TestInstalledEntrypointBindsNativeCommerce(t *testing.T) {
	storageHost(t)
	binary := filepath.Join(t.TempDir(), "payments")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/payments")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build entrypoint: %v %s", err, output)
	}
	cmd := exec.Command(binary)
	cmd.Env = []string{"ZBOARD_PLUGIN_HOST_SOCKET=" + os.Getenv("ZBOARD_PLUGIN_HOST_SOCKET"), "ZBOARD_PLUGIN_HOST_TOKEN=" + os.Getenv("ZBOARD_PLUGIN_HOST_TOKEN")}
	client := hcplugin.NewClient(&hcplugin.ClientConfig{HandshakeConfig: pluginv1.Handshake, Plugins: pluginv1.ClientMap(), Cmd: cmd, AllowedProtocols: []hcplugin.Protocol{hcplugin.ProtocolGRPC}, AutoMTLS: true, SkipHostEnv: true, StartTimeout: 10 * time.Second, Logger: hclog.NewNullLogger(), SyncStdout: io.Discard, SyncStderr: io.Discard})
	defer client.Kill()
	rpc, err := client.Client()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := rpc.Dispense("control")
	if err != nil {
		t.Fatal(err)
	}
	api := raw.(pluginv1.PluginControlClient)
	info, err := api.GetInfo(t.Context(), &pluginv1.Empty{})
	if err != nil || info.Id != pluginID || len(info.Capabilities) != len(pluginCapabilities) {
		t.Fatalf("entrypoint identity: %+v %v", info, err)
	}
	health, err := api.Health(t.Context(), &pluginv1.Empty{})
	if err != nil || !health.Healthy {
		t.Fatalf("entrypoint health: %+v %v", health, err)
	}
	response, err := api.HandlePageAction(t.Context(), &pluginv1.PageActionRequest{Surface: "account", ActorId: "buyer", Action: "payments.status", PayloadJson: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Data Readiness `json:"data"`
	}
	if err := json.Unmarshal(response.ResultJson, &result); err != nil || !result.Data.StorageReady || !result.Data.CheckoutReady || len(result.Data.Missing) != 0 {
		t.Fatalf("actual process readiness: %s %v", response.ResultJson, err)
	}
}
