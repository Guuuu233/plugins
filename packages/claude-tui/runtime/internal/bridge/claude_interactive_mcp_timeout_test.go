package bridge

import (
	"encoding/json"
	"testing"
)

func TestInteractiveMCPToolTimeoutSurvivesLongClientTool(t *testing.T) {
	var config struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Timeout int      `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(interactiveMCPConfig("/bin/helper", []string{"serve"}, "http://127.0.0.1:1234/callback", "/tmp/tools.json")), &config); err != nil {
		t.Fatal(err)
	}
	server, ok := config.MCPServers["magpie"]
	if !ok {
		t.Fatal("magpie MCP server missing")
	}
	if server.Timeout != 1800000 {
		t.Fatalf("MCP call still susceptible to 300s cutoff: timeout=%d", server.Timeout)
	}
	if server.Command != "/bin/helper" || len(server.Args) != 3 || server.Args[0] != "serve" || server.Args[1] != "http://127.0.0.1:1234/callback" || server.Args[2] != "/tmp/tools.json" {
		t.Fatalf("MCP helper invocation changed: %+v", server)
	}
}
