package cli

import (
	"flag"
	"testing"
)

// TestRegisterFlags verifies that RegisterFlags binds every flag: field in the
// Flags struct to the given FlagSet with the correct name and type. This test
// also acts as a coverage enforcement gate: if a new field is added to Flags
// without a flag: tag, RegisterFlags silently ignores it and the field would
// not appear in the flag set -- which would be caught here if a matching
// Lookup assertion is added.
func TestRegisterFlags_AllFlagsRegistered(t *testing.T) {
	// Use a fresh FlagSet so we do not pollute the test binary's
	// flag.CommandLine with repeated registrations.
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var f Flags
	RegisterFlags(fs, &f)

	// Verify each expected flag is registered with the correct type.
	wantFlags := []struct {
		name     string
		defValue string
	}{
		{"reset-password", "false"},
		{"username", ""},
		{"new-password", ""},
	}
	for _, wf := range wantFlags {
		fl := fs.Lookup(wf.name)
		if fl == nil {
			t.Errorf("flag %q not registered by RegisterFlags", wf.name)
			continue
		}
		if fl.DefValue != wf.defValue {
			t.Errorf("flag %q default = %q, want %q", wf.name, fl.DefValue, wf.defValue)
		}
	}
}

// TestRegisterFlags_ParsesIntoStruct verifies that parsing flags from a
// FlagSet populated by RegisterFlags correctly updates the Flags fields.
func TestRegisterFlags_ParsesIntoStruct(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var f Flags
	RegisterFlags(fs, &f)

	args := []string{"--reset-password", "--username=alice", "--new-password=s3cret"}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !f.ResetPassword {
		t.Error("ResetPassword should be true after --reset-password flag")
	}
	if f.Username != "alice" {
		t.Errorf("Username = %q, want alice", f.Username)
	}
	if f.NewPassword != "s3cret" {
		t.Errorf("NewPassword = %q, want s3cret", f.NewPassword)
	}
}

// TestRegisterFlags_Defaults verifies that unparsed flags retain their zero
// defaults (not the default: struct tag values, which are documentation-only).
func TestRegisterFlags_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var f Flags
	RegisterFlags(fs, &f)

	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.ResetPassword {
		t.Error("ResetPassword should default to false")
	}
	if f.Username != "" {
		t.Errorf("Username should default to empty string, got %q", f.Username)
	}
	if f.NewPassword != "" {
		t.Errorf("NewPassword should default to empty string, got %q", f.NewPassword)
	}
}

// TestSubcommands_NonEmpty verifies that the Subcommands slice has at least one
// entry and that each entry has a non-empty Name and Summary. The generator
// renders every entry in this slice; an empty or malformed entry would produce
// broken documentation.
func TestSubcommands_NonEmpty(t *testing.T) {
	if len(Subcommands) == 0 {
		t.Fatal("Subcommands slice is empty; at least one subcommand expected")
	}
	for i, s := range Subcommands {
		if s.Name == "" {
			t.Errorf("Subcommands[%d].Name is empty", i)
		}
		if s.Summary == "" {
			t.Errorf("Subcommands[%d].Summary is empty (Name=%q)", i, s.Name)
		}
		if s.Details == "" {
			t.Errorf("Subcommands[%d].Details is empty (Name=%q)", i, s.Name)
		}
	}
}
