package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"reasonix/internal/sessioncatalog"
)

func TestListCatalogSessionsForDirectoryMatchesEquivalentWindowsPathSpellings(t *testing.T) {
	workspaceRoot := t.TempDir()
	sessionDir := desktopSessionDir(workspaceRoot)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := writeTopicSessionWithPrompt(t, sessionDir, "case.jsonl", "topic_case", "Case topic",
		workspaceRoot, "case prompt", time.Now())
	catalog, err := sessioncatalog.Open(context.Background(), sessioncatalog.Options{InMemory: true, DisableRepair: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close(context.Background()) })
	if err := catalog.ReconcileDirectory(context.Background(), sessioncatalog.DirectoryTarget{
		Path: sessionDir, Scope: "project", WorkspaceRoot: workspaceRoot,
	}); err != nil {
		t.Fatal(err)
	}

	records, err := listCatalogSessionsForDirectory(context.Background(), catalog, sessioncatalog.DirectoryTarget{
		Path: sessionDir, Scope: "project", WorkspaceRoot: strings.ToUpper(workspaceRoot),
	}, strings.ToUpper(sessionDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !sameDesktopPath(records[0].Path, sessionPath) {
		t.Fatalf("equivalent Windows path query = %+v, want %q", records, sessionPath)
	}
}
