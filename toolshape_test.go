package main

import (
	"strings"
	"testing"
)

func TestToolWireNameIsOpaque(t *testing.T) {
	used := map[string]bool{}
	for _, client := range []string{"ScheduleWakeup", "ReportFindings", "Read", "Bash", "subagent_fork", "ops_bash"} {
		wireName := ClientToolWireName(client, used)
		if !strings.HasPrefix(wireName, ClientToolPrefix) {
			t.Errorf("wire name %q does not carry the client prefix", wireName)
		}
		if len(wireName) > toolWireNameLimit {
			t.Errorf("wire name %q is longer than %d bytes", wireName, toolWireNameLimit)
		}
		if strings.Contains(strings.ToLower(wireName), strings.ToLower(client)) {
			t.Errorf("wire name %q still spells the client name %q", wireName, client)
		}
	}
}

func TestToolRenamesRestoreOpaqueNames(t *testing.T) {
	renames := NewToolRenames()
	used := map[string]bool{}
	for _, client := range []string{"ReportFindings", "ScheduleWakeup", "subagent_fork"} {
		wireName := ClientToolWireName(client, used)
		if wireName == client {
			t.Fatalf("%q was not renamed", client)
		}
		renames.Add(client, wireName)
		if got := renames.Restore(wireName); got != client {
			t.Errorf("Restore(%q) = %q, want %q", wireName, got, client)
		}
	}
}
