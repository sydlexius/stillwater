package cli

import (
	"flag"
	"reflect"
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
	if err := RegisterFlags(fs, &f); err != nil {
		t.Fatalf("RegisterFlags: %v", err)
	}

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
	if err := RegisterFlags(fs, &f); err != nil {
		t.Fatalf("RegisterFlags: %v", err)
	}

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

// TestRegisterFlags_Defaults verifies that unparsed flags retain the defaults
// declared via the default: struct tags. For the current Flags struct those
// defaults happen to equal each type's zero value (false / ""), so this also
// confirms the parsed tag value and the zero value agree here.
func TestRegisterFlags_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var f Flags
	if err := RegisterFlags(fs, &f); err != nil {
		t.Fatalf("RegisterFlags: %v", err)
	}

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

// TestRegisterFlags_NilArgs verifies that RegisterFlags fails loudly (returns a
// non-nil error) rather than panicking when handed a nil flag set or nil Flags.
func TestRegisterFlags_NilArgs(t *testing.T) {
	var f Flags
	if err := RegisterFlags(nil, &f); err == nil {
		t.Error("RegisterFlags(nil fs) should return an error")
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	if err := RegisterFlags(fs, nil); err == nil {
		t.Error("RegisterFlags(nil f) should return an error")
	}
}

// TestRegisterStructFlags_ErrorBranches exercises the fail-loud error branches
// of the reflection core directly, using deliberately malformed local structs
// that the exported Flags type cannot express (an unsupported field kind and an
// unparsable bool default). These paths guard against documented defaults and
// runtime defaults silently drifting apart.
func TestRegisterStructFlags_ErrorBranches(t *testing.T) {
	// (a) A flag-tagged field of an unsupported kind (int) must error.
	type unsupportedKind struct {
		Count int `flag:"count" default:"0" desc:"a count"`
	}
	// (b) A bool field with an unparsable default must error.
	type badBoolDefault struct {
		On bool `flag:"on" default:"notabool" desc:"a toggle"`
	}
	// (c) A named-bool field (Kind()==Bool but concrete pointer is *myBool,
	// not *bool) must hit the guarded type-assertion error rather than panic.
	type myBool bool
	type namedBoolField struct {
		On myBool `flag:"on" default:"false" desc:"a toggle"`
	}
	// (d) A named-string field (Kind()==String but concrete pointer is
	// *myString, not *string) must likewise hit the guarded error path.
	type myString string
	type namedStringField struct {
		Name myString `flag:"name" default:"x" desc:"a name"`
	}
	// (e) A well-formed struct must register without error.
	type validStruct struct {
		Verbose bool   `flag:"verbose" default:"true" desc:"verbose output"`
		Name    string `flag:"name" default:"world" desc:"a name"`
	}

	tests := []struct {
		name    string
		val     any
		wantErr bool
	}{
		{"unsupported kind", unsupportedKind{}, true},
		{"invalid bool default", badBoolDefault{}, true},
		{"named bool type", namedBoolField{}, true},
		{"named string type", namedStringField{}, true},
		{"valid struct", validStruct{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			// Take an addressable copy so Field().Addr() works inside the helper.
			rv := reflect.New(reflect.TypeOf(tc.val)).Elem()
			rv.Set(reflect.ValueOf(tc.val))
			err := registerStructFlags(fs, rv.Type(), rv)
			if tc.wantErr && err == nil {
				t.Errorf("registerStructFlags(%s) = nil error, want non-nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("registerStructFlags(%s) = %v, want nil", tc.name, err)
			}
		})
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
