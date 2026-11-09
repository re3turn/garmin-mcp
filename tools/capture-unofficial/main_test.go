package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalando/go-keyring"

	"github.com/re3turn/garmin-mcp/internal/garmin"
	"github.com/re3turn/garmin-mcp/internal/server"
)

// TestMain keeps every test away from the real macOS keychain and Windows
// Credential Manager.
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

// dependentCaptures read one item from a list captureList records, so run
// records them after the list instead of through captureList.
var dependentCaptures = []string{"garmin_get_training_plan", "garmin_get_workout"}

// A tool added to the server without a capture here would never have its
// shape recorded.
func TestEveryUnofficialToolIsCaptured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(garmin.KeyEnvVar, "test-key")
	connect, err := garmin.NewConnectClient()
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.New(garmin.NewClient(), "test", server.WithUnofficial(connect)).Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var served []string
	for _, tool := range list.Tools {
		if strings.Contains(tool.Description, "Unofficial Garmin Connect API") {
			served = append(served, tool.Name)
		}
	}

	captured := slices.Clone(dependentCaptures)
	for _, item := range captureList(connect, time.Now()) {
		name, _, _ := strings.Cut(item.name, ".")
		if !slices.Contains(captured, name) {
			captured = append(captured, name)
		}
	}
	slices.Sort(served)
	slices.Sort(captured)
	if !slices.Equal(served, captured) {
		t.Errorf("captured tools = %v, want the served ones %v", captured, served)
	}
}
