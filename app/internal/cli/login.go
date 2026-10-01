package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/gvinsot/Probe/app/internal/hublogin"
)

// newHubClient is replaced in tests.
var newHubClient = hublogin.NewClient

// loginCommand connects this machine to a Probe Hub account, or reports the
// current login with --status.
func loginCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("login", flag.ContinueOnError)
	f.SetOutput(errOut)
	hubFlag := f.String("hub", "", "Probe Hub URL (default "+hublogin.HubURLEnv+" or "+hublogin.DefaultHub+")")
	status := f.Bool("status", false, "show the current login and the remaining daily quota")
	if err := f.Parse(args); err != nil {
		return flagCode(err)
	}
	if f.NArg() != 0 {
		return fail(errOut, 3, "login takes no arguments")
	}
	if *status {
		return loginStatus(ctx, out, errOut)
	}
	hub := *hubFlag
	if hub == "" {
		hub = os.Getenv(hublogin.HubURLEnv)
	}
	if hub == "" {
		hub = hublogin.DefaultHub
	}
	path := hublogin.Path(os.Getenv)
	if path == "" {
		return fail(errOut, 3, "nowhere to store the login: %s=off or no user configuration directory", hublogin.CredentialsFileEnv)
	}
	host, _ := os.Hostname()
	client := "Probe CLI"
	if host != "" {
		client += " on " + host
	}
	client += " (" + runtime.GOOS + ")"
	creds, err := newHubClient().Login(ctx, hub, client, func(p hublogin.Prompt) {
		link := p.CompleteURI
		if link == "" {
			link = p.VerificationURI
		}
		fmt.Fprintf(out, "To connect this machine to the hub, open this page in a browser where you are signed in:\n\n  %s\n\nand check that it shows the code %s. The code expires in %s.\nWaiting for approval (Ctrl+C to cancel)...\n", link, p.UserCode, p.ExpiresIn.Round(time.Minute))
	})
	if err != nil {
		return fail(errOut, 3, "login failed: %v", err)
	}
	if err := hublogin.Save(path, creds); err != nil {
		return fail(errOut, 3, "login succeeded, but the credentials could not be saved to %s: %v", path, err)
	}
	who := creds.Login
	if creds.Provider != "" {
		who += " (" + creds.Provider + ")"
	}
	fmt.Fprintf(out, "Logged in to %s as %s.\n", creds.Hub, who)
	fmt.Fprintf(out, "probe review, plan and knowledge build now use the hub's LLM (%s) when no provider endpoint or key is configured; the source context they send goes to the hub.\n", creds.Model)
	fmt.Fprintf(out, "Credentials: %s. Revoke them with probe logout.\n", path)
	return 0
}

func loginStatus(ctx context.Context, out, errOut io.Writer) int {
	if os.Getenv(hublogin.HubTokenEnv) != "" {
		fmt.Fprintf(out, "%s is set: it takes precedence over the stored login.\n", hublogin.HubTokenEnv)
	}
	creds, path, found, err := hublogin.Load(os.Getenv, os.ReadFile)
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if !found {
		fmt.Fprintln(out, "Not logged in. Run probe login.")
		return 1
	}
	fmt.Fprintf(out, "Hub: %s\nCredentials: %s\n", creds.Hub, path)
	if !creds.ExpiresAt.IsZero() {
		fmt.Fprintf(out, "Expires: %s\n", creds.ExpiresAt.Local().Format("2006-01-02 15:04"))
		if time.Now().After(creds.ExpiresAt) {
			fmt.Fprintln(out, "This login has expired. Run probe login again.")
			return 1
		}
	}
	account, err := newHubClient().Account(ctx, creds)
	if err != nil {
		return fail(errOut, 1, "%v", err)
	}
	fmt.Fprintf(out, "Account: %s (%s)\nModel: %s\nToday: %d of %d tokens used, resets %s\n", account.Login, account.Provider, account.Model, account.UsedToday, account.DailyTokens, account.ResetsAt.Local().Format("2006-01-02 15:04"))
	return 0
}

// logoutCommand revokes the stored token on the hub and deletes it.
func logoutCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	f := flag.NewFlagSet("logout", flag.ContinueOnError)
	f.SetOutput(errOut)
	if err := f.Parse(args); err != nil {
		return flagCode(err)
	}
	if f.NArg() != 0 {
		return fail(errOut, 3, "logout takes no arguments")
	}
	creds, path, found, err := hublogin.Load(os.Getenv, os.ReadFile)
	if err != nil && path != "" {
		// An unreadable file holds no token worth revoking; remove it anyway.
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fail(errOut, 3, "%v", rmErr)
		}
		fmt.Fprintf(out, "Removed the unusable credentials %s.\n", path)
		return 0
	}
	if err != nil {
		return fail(errOut, 3, "%v", err)
	}
	if !found {
		fmt.Fprintln(out, "Not logged in.")
		return 0
	}
	code := 0
	if err := newHubClient().Revoke(ctx, creds); err != nil {
		fmt.Fprintf(errOut, "The hub could not revoke the token (%v): revoke it from Agent access in the dashboard.\n", err)
		code = 1
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(errOut, 3, "%v", err)
	}
	fmt.Fprintf(out, "Logged out of %s.\n", creds.Hub)
	if os.Getenv(hublogin.HubTokenEnv) != "" {
		fmt.Fprintf(out, "%s is still set in this environment.\n", hublogin.HubTokenEnv)
	}
	return code
}
