// Package cli defines the structured metadata for Stillwater's command-line
// flags. The Flags struct uses struct tags to declare each flag's name, default
// value, and user-facing description -- the same pattern config.Config uses for
// environment variables. The cmd/gen-cli-reference generator reflects over this
// struct to produce the CLI flags reference page; adding a new flag to Flags
// automatically includes it in the generated docs and fails the generator if
// its desc: tag is missing (coverage enforcement).
//
// Usage in the main binary:
//
//	var f cli.Flags
//	if err := cli.RegisterFlags(flag.CommandLine, &f); err != nil { ... }
//	flag.Parse()
//	if f.ResetPassword { ... }
package cli

import (
	"errors"
	"flag"
	"fmt"
	"reflect"
	"strconv"
)

// Flags holds all global CLI flags accepted by the stillwater binary.
// Each field carries three struct tags:
//
//   - flag:    the exact flag name users pass on the command line
//   - default: the default value as a string. RegisterFlags parses this and
//     passes it to the flag package, so it is the actual runtime default the
//     flag holds when unset (not merely documentation); it also appears in the
//     generated docs.
//   - desc:    a one-sentence user-facing description (required; the generator
//     fails loudly if absent, enforcing coverage for every flag)
type Flags struct {
	// ResetPassword, when true, resets the admin user password and exits.
	// The --username and --new-password flags control whose password is changed
	// and how the new value is supplied.
	ResetPassword bool `flag:"reset-password" default:"false" desc:"Reset the admin user password and exit. Prompts interactively unless --new-password is also set."`

	// Username specifies which user account to target for --reset-password.
	// When empty, Stillwater picks the only admin user in the database, or
	// returns an error if more than one admin exists.
	Username string `flag:"username" default:"" desc:"Username for --reset-password. When omitted, defaults to the sole admin user in the database."`

	// NewPassword, when set, supplies the new password for --reset-password
	// without an interactive prompt. Exposing a password via a command-line
	// argument is insecure because it appears in process listings (ps, top,
	// /proc); prefer the interactive prompt whenever possible.
	NewPassword string `flag:"new-password" default:"" desc:"New password for --reset-password (INSECURE: visible in process listings; prefer the interactive prompt instead)."`

	// LockDamageDryRun, when true, runs the locked-field damage repair in
	// report-only mode and exits: it selects and prints the candidate report,
	// writes nothing, does not record completion, and never starts a listener.
	// Built for validating the repair predicate against a database copy before
	// letting a real pass write (issue #3038 / #3075).
	LockDamageDryRun bool `flag:"lock-damage-dry-run" default:"false" desc:"Report which locked fields the damage repair would restore, then exit without writing. For validating against a database copy."`

	// LockDamagePreGuardDryRun previews the PRE-GUARD population (issue
	// #3079): locked fields overwritten before the persist chokepoint
	// shipped, whose history rows name no rule and so can never be
	// attributed. Report-only; writes nothing and records no completion.
	LockDamagePreGuardDryRun bool `flag:"lock-damage-pre-guard-dry-run" default:"false" desc:"Report which locked fields the pre-guard (unattributable) damage repair would restore, then exit without writing. Preview this before --lock-damage-pre-guard-repair."`

	// LockDamagePreGuardRepair runs the pre-guard repair for real, once per
	// database, then exits.
	//
	// A SEPARATE EXPLICIT FLAG AND NOT A STARTUP ONE-SHOT, deliberately.
	// #3074 rejected an automatic restore of unattributable damage because a
	// standing mechanism cannot tell rule damage from an operator's own edit;
	// what answers that is a closed population plus a HUMAN RULING ON THE CUT
	// before anything writes, and a boot-time pass has nowhere to put that
	// ruling. The pass cannot write unless somebody typed this.
	//
	// UNLIKE THE DRY RUN, THIS OPENS THE DATABASE READ-WRITE AND MIGRATES IT.
	// It must not be pointed at the database of a running server.
	LockDamagePreGuardRepair bool `flag:"lock-damage-pre-guard-repair" default:"false" desc:"Restore the pre-guard (unattributable) locked-field damage previewed by --lock-damage-pre-guard-dry-run, then exit. Requires --lock-damage-pre-guard-approve with the digest that preview printed. Runs once per database, opens the database READ-WRITE and RUNS MIGRATIONS, so stop the server first and back up. Preview first."`

	// LockDamagePreGuardApprove carries the approval digest the pre-guard dry
	// run printed for the candidate set the operator reviewed.
	//
	// THIS IS WHAT MAKES THE PREVIEW BINDING (#3079 review). The dry run and
	// the repair are separate invocations, each re-selecting from scratch, so
	// without a token carried between them the write restores whatever the
	// predicate matches at write time -- and selection condition 1 is "locked
	// NOW", so a single lock toggled between the two pulls unpreviewed damage
	// into the write. The repair refuses to write unless the set it selected
	// digests to this value.
	//
	// NOT a boolean "I approve": a boolean would confirm the operator's
	// intent while saying nothing about WHICH rows they intended.
	LockDamagePreGuardApprove string `flag:"lock-damage-pre-guard-approve" default:"" desc:"The approval digest printed by --lock-damage-pre-guard-dry-run. Required by --lock-damage-pre-guard-repair, which refuses to write if the candidate set no longer matches it."`
}

// SubcommandInfo describes a CLI subcommand (os.Args[1] dispatch) that
// Stillwater recognizes. Subcommands are handled before flag parsing and
// accept no additional flags themselves.
type SubcommandInfo struct {
	// Name is the exact string to pass as the first argument (e.g.
	// "reset-credentials").
	Name string

	// Summary is a one-sentence description of what the subcommand does.
	Summary string

	// Details is a longer description including behavior and caveats,
	// formatted as plain prose for rendering in the docs page.
	Details string
}

// Subcommands lists every subcommand the stillwater binary recognizes.
// The generator reads this slice to produce the "Subcommands" section
// of the CLI reference page.
var Subcommands = []SubcommandInfo{
	{
		Name:    "reset-credentials",
		Summary: "Wipe all stored credentials and force a fresh setup on next start.",
		Details: "Clears all provider API keys, connection credentials, user accounts, and " +
			"active sessions from the database. Use this when the encryption key is lost " +
			"or credentials need to be re-entered from scratch. The application will prompt " +
			"for initial setup on the next start. Requires database access (SW_DB_PATH or " +
			"SW_CONFIG_PATH must resolve to the live database).",
	},
}

// RegisterFlags binds the fields of f to the given flag set using the flag:
// and default: struct tags. Call this before flag.Parse(). The default: tag
// supplies each flag's default value: RegisterFlags parses it and passes it to
// the flag package's *Var call (BoolVar/StringVar), so it IS the value the flag
// holds when the user does not pass the flag on the command line. (A bool's
// default: tag is parsed with strconv.ParseBool; a string's is used verbatim.)
//
// RegisterFlags returns an error on misconfigured tag metadata -- an
// unsupported field kind or an unparsable bool default -- so the binary fails
// loudly at startup rather than letting the documented defaults and the runtime
// defaults silently drift apart. It nil-guards both arguments.
func RegisterFlags(fs *flag.FlagSet, f *Flags) error {
	if fs == nil {
		return errors.New("RegisterFlags: nil flag set")
	}
	if f == nil {
		return errors.New("RegisterFlags: nil Flags")
	}
	return registerStructFlags(fs, reflect.TypeOf(*f), reflect.ValueOf(f).Elem())
}

// registerStructFlags reflects over the fields of the struct described by t/v
// and registers a flag for each field carrying a flag: tag. It is the
// unexported core of RegisterFlags, split out so its error branches are
// reachable from tests with deliberately malformed structs.
func registerStructFlags(fs *flag.FlagSet, t reflect.Type, v reflect.Value) error {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		flagName := field.Tag.Get("flag")
		desc := field.Tag.Get("desc")
		if flagName == "" {
			continue
		}

		fv := v.Field(i)
		switch field.Type.Kind() {
		case reflect.Bool:
			raw := field.Tag.Get("default")
			if raw == "" {
				raw = "false"
			}
			defaultVal, err := strconv.ParseBool(raw)
			if err != nil {
				return fmt.Errorf("field %s has invalid bool default %q: %w", field.Name, raw, err)
			}
			// Guard the type assertion: Kind()==Bool also matches named types
			// (e.g. `type myBool bool`) whose concrete pointer is *myBool, not
			// *bool. A bare cast would panic on those, violating this function's
			// fail-loud contract. Use the comma-ok form and return an error
			// instead so a misdeclared field surfaces at startup, not as a panic.
			ptr, ok := fv.Addr().Interface().(*bool)
			if !ok {
				return fmt.Errorf("field %s for flag %q must be builtin bool, got %s", field.Name, flagName, field.Type)
			}
			fs.BoolVar(ptr, flagName, defaultVal, desc)
		case reflect.String:
			defaultVal := field.Tag.Get("default")
			ptr, ok := fv.Addr().Interface().(*string)
			if !ok {
				return fmt.Errorf("field %s for flag %q must be builtin string, got %s", field.Name, flagName, field.Type)
			}
			fs.StringVar(ptr, flagName, defaultVal, desc)
		default:
			return fmt.Errorf("field %s has unsupported kind %s for flag %q", field.Name, field.Type.Kind(), flagName)
		}
	}
	return nil
}
