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
//	cli.RegisterFlags(flag.CommandLine, &f)
//	flag.Parse()
//	if f.ResetPassword { ... }
package cli

import (
	"flag"
	"reflect"
)

// Flags holds all global CLI flags accepted by the stillwater binary.
// Each field carries three struct tags:
//
//   - flag:    the exact flag name users pass on the command line
//   - default: the default value as a string (used in generated docs)
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
// and default: struct tags. Call this before flag.Parse(). All flags receive
// their zero-value defaults when the corresponding flag is not passed; the
// default: tag is documentation-only and does not affect runtime behavior
// (the Go flag package uses the zero value of the type for unset flags unless
// an explicit default is passed to the flag registration call, which this
// function derives from the tag).
func RegisterFlags(fs *flag.FlagSet, f *Flags) {
	t := reflect.TypeOf(*f)
	v := reflect.ValueOf(f).Elem()

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		flagName := field.Tag.Get("flag")
		desc := field.Tag.Get("desc")
		if flagName == "" {
			continue
		}

		fv := v.Field(i)
		switch field.Type.Kind() { //nolint:exhaustive // only bool and string are used today
		case reflect.Bool:
			defaultVal := field.Tag.Get("default") == "true"
			// Addr().Interface() returns the concrete *bool pointer so we can
			// pass it directly to BoolVar without importing unsafe.
			fs.BoolVar(fv.Addr().Interface().(*bool), flagName, defaultVal, desc)
		case reflect.String:
			defaultVal := field.Tag.Get("default")
			fs.StringVar(fv.Addr().Interface().(*string), flagName, defaultVal, desc)
		}
	}
}
