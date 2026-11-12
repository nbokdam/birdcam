package agent

import (
	"os"
	"testing"
)

// The shipped example must stay a valid config.
func TestExampleConfigParses(t *testing.T) {
	b, err := os.ReadFile("../../deploy/agent.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	if _, err := ParseConfig(b); err != nil {
		t.Fatal(err)
	}
}
