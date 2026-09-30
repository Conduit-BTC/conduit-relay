package conduitl2

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPublicAgentCredentialBoundary(t *testing.T) {
	const directory = ".github/workflows"
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	accountExecution := regexp.MustCompile(`(?i)CODEX_AUTH_JSON|WORKFLOW_AGENT_GITHUB_APP_(?:PRIVATE_KEY|CLIENT_ID)|sudden-network/agent`)
	workflows := 0
	for _, entry := range entries {
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if entry.IsDir() || (extension != ".yml" && extension != ".yaml") {
			continue
		}
		workflows++
		workflow, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if accountExecution.Match(workflow) {
			t.Errorf("%s must keep account-authenticated agent execution in the private runner", entry.Name())
		}
	}
	if workflows == 0 {
		t.Fatal("expected public CI workflows to verify")
	}
}
