package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/MarioGK/t3-agent37/internal/peers"
)

const (
	templateGuide    = "https://github.com/MarioGK/t3-agent37/blob/main/docs/terrascale.md"
	tailscaleGuide   = "https://console.tailscale.com/admin/settings/trust-credentials"
	tailscalePolicy  = "https://console.tailscale.com/admin/acls"
	tailscaleDNS     = "https://login.tailscale.com/admin/dns"
	agent37Dashboard = "https://www.agent37.com/dashboard/cloud"
)

func setupWorkstation(ctx context.Context, opts options, output, diagnostic io.Writer) error {
	interactive := terminalAvailable(diagnostic)
	if !opts.checkSetup && !opts.verifySetup && !interactive {
		return errors.New("setup requires a terminal. Use setup --check to inspect this workstation")
	}
	config, err := readWorkstationConfig()
	if err != nil {
		return err
	}
	runtime, err := readRuntimeConfig()
	if err != nil {
		return err
	}
	key, err := existingAgent37APIKey()
	if err != nil {
		return err
	}
	key = cmp.Or(key, runtime.value(agent37KeyName))
	openBrowser := interactive && !opts.noSetupBrowser
	if !opts.checkSetup && !opts.verifySetup {
		if openBrowser {
			openTailscaleConsole(diagnostic)
		}
		return editWorkstationSetup(ctx, config, runtime, key, output, diagnostic)
	}
	ready := setupStatus(output, config.values, runtime, key)
	if opts.verifySetup {
		verified, needsConsole := verifySetupTailscale(ctx, output, config.values.TailscaleTags, runtime)
		if openBrowser && needsConsole {
			openTailscaleConsole(diagnostic)
		}
		if !ready || !verified {
			return errors.New("workstation verification is incomplete")
		}
		fmt.Fprintln(output, "Run t3-agent37 up to provision a sandbox and verify access.")
		return nil
	}
	if !ready {
		return errors.New("workstation setup is incomplete")
	}
	fmt.Fprintln(output, "Run t3-agent37 setup --verify to check Tailscale.")
	return nil
}

func editWorkstationSetup(ctx context.Context, config configFile, runtime runtimeConfig, key string, output, diagnostic io.Writer) error {
	values := &config.values
	tagInput := cmp.Or(values.TailscaleTags, peers.DefaultEnrollmentTag)
	usesCLIProxy := func() bool { return sandboxProvider(values.Provider, values.Template) == "cliproxy" }
	var apiKey, clientID, clientSecret, baseURL, clientKey string
	var save bool
	var groups []*huh.Group
	if key == "" {
		groups = append(groups, credentialInput("Agent37 API key", "Create a workspace API key at "+agent37Dashboard, &apiKey))
	}
	groups = append(groups,
		sandboxDefaults(values, "Enter a template with Tailscale installed. Add @revision to pin it.\nTo publish the TerraScale template, see "+templateGuide),
		providerOptions(values),
		huh.NewGroup(huh.NewNote().Title("Private Tailscale access").
			Description("In Trust credentials, edit your OAuth client or choose Credential → OAuth.\n"+
				"Set Auth Keys to Write and select the tags this sandbox will use.\n"+
				"Define new tags and grant T3 access on port 443 at "+tailscalePolicy+"\n"+
				"Enable MagicDNS and HTTPS certificates at "+tailscaleDNS+"\n"+tailscaleGuide).
			Next(true),
		),
		huh.NewGroup(huh.NewInput().Title("Tailscale enrollment tags").
			Description("Enter the OAuth client's exact tag set, separated by commas.").
			Value(&tagInput).Validate(func(value string) error {
			_, err := peers.ParseEnrollmentTags(value)
			return err
		})),
		credentialInput("Tailscale OAuth client ID", "Use the client ID from Trust credentials. Leave blank to keep the saved value.", &clientID).
			WithHide(strings.TrimSpace(os.Getenv(tailClientID)) != ""),
		credentialInput("Tailscale OAuth client secret", "Use the secret from the same OAuth client. Leave blank to keep the saved value.", &clientSecret).
			WithHide(strings.TrimSpace(os.Getenv(tailSecret)) != ""),
		huh.NewGroup(huh.NewInput().Title("CLIProxy base URL").
			Description("Use the HTTPS endpoint for your model catalog.").
			Value(&baseURL).Validate(validateCLIProxyInput)).
			WithHideFunc(func() bool { return !usesCLIProxy() || runtime.value(proxyURL) != "" }),
		credentialInput("CLIProxy client API key", "Use a client key for the CLIProxy models endpoint.", &clientKey).
			WithHideFunc(func() bool { return !usesCLIProxy() || runtime.value(proxyKey) != "" }),
		huh.NewGroup(huh.NewConfirm().
			Title("Save workstation setup?").
			Description("You can leave fields blank and finish setup later.").
			Affirmative("Save").
			Negative("Cancel").
			Value(&save)),
	)
	form := huh.NewForm(groups...).WithInput(os.Stdin).WithOutput(diagnostic)
	err := form.RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) || (err == nil && !save) {
		fmt.Fprintln(diagnostic, "Setup canceled.")
		return nil
	}
	if err != nil {
		return err
	}
	tags, err := peers.ParseEnrollmentTags(tagInput)
	if err != nil {
		return err
	}

	updates := map[string]string{agent37KeyName: apiKey, tailClientID: clientID, tailSecret: clientSecret}
	if usesCLIProxy() {
		updates[proxyURL], updates[proxyKey] = baseURL, clientKey
	}
	changed := false
	for name, value := range updates {
		if value = strings.TrimSpace(value); value != "" {
			runtime.values[name] = value
			changed = true
		}
	}
	if changed {
		if err := saveConfigJSON(ctx, runtime.path, runtime.original, runtime.values); err != nil {
			return err
		}
		fmt.Fprintln(output, "Saved credentials to", runtime.path)
		key = cmp.Or(key, runtime.value(agent37KeyName))
	}
	values.Template = strings.TrimSpace(values.Template)
	values.Tailscale = nil
	values.TailscaleTags = strings.Join(tags, ",")
	if err := config.save(ctx); err != nil {
		return err
	}
	fmt.Fprintln(output, "Saved sandbox defaults to", config.path)
	if setupStatus(output, *values, runtime, key) {
		fmt.Fprintln(output, "Run t3-agent37 setup --verify to check Tailscale.")
	}
	return nil
}

func credentialInput(title, description string, value *string) *huh.Group {
	return huh.NewGroup(huh.NewInput().Title(title).Description(description).
		EchoMode(huh.EchoModePassword).Value(value))
}

func validateCLIProxyInput(value string) error {
	if value = strings.TrimSpace(value); value != "" && !validCLIProxyURL(value) {
		return errors.New("use an HTTPS base URL without credentials, a query, or a fragment")
	}
	return nil
}

func validCLIProxyURL(value string) bool {
	endpoint, err := url.Parse(value)
	return err == nil && endpoint.Scheme == "https" && endpoint.Hostname() != "" &&
		endpoint.User == nil && endpoint.RawQuery == "" && !endpoint.ForceQuery && !strings.Contains(value, "#")
}

func setupStatus(output io.Writer, config workstationConfig, runtime runtimeConfig, agent37Key string) bool {
	fmt.Fprintln(output, "Workstation configuration")
	ready := true
	report := func(ok bool, name, detail string) {
		if ok {
			fmt.Fprintln(output, "  Configured", name)
		} else {
			ready = false
			fmt.Fprintf(output, "  Needs attention %s. %s\n", name, detail)
		}
	}
	report(agent37Key != "", "Agent37 API key", "Create a workspace API key at "+agent37Dashboard)
	report(strings.TrimSpace(config.Template) != "", "Agent37 template", "Choose a Tailscale-ready template. "+templateGuide)
	report(validateNamePrefix(config.NamePrefix) == nil, "Sandbox name prefix", "Use t3-agent37 config to set a valid prefix.")
	report(config.Tailscale == nil || *config.Tailscale, "Tailscale enrollment", "Run t3-agent37 setup to enable it.")
	tags, tagErr := peers.ParseEnrollmentTags(config.TailscaleTags)
	if tagErr != nil {
		report(false, "Tailscale enrollment tags", tagErr.Error()+". Run t3-agent37 setup.")
	} else {
		fmt.Fprintln(output, "  Configured Tailscale enrollment tags", strings.Join(tags, ", "))
	}
	report(runtime.value(tailClientID) != "" && runtime.value(tailSecret) != "", "Tailscale OAuth client",
		"Create a client with Auth Keys Write and the configured tags. "+tailscaleGuide)
	provider := sandboxProvider(config.Provider, config.Template)
	_, supported := providers[provider]
	report(supported, "Model provider", "Choose a supported provider in t3-agent37 config.")
	if provider == "cliproxy" {
		report(validCLIProxyURL(runtime.value(proxyURL)), "CLIProxy HTTPS base URL", "Set T3_AGENT37_CLIPROXY_URL.")
		report(runtime.value(proxyKey) != "", "CLIProxy client API key", "Set T3_AGENT37_CLIPROXY_KEY.")
	}
	configured, err := peers.Configured()
	if err != nil {
		report(false, "T3 peers", err.Error()+". "+templateGuide+"#link-t3-servers")
	} else if len(configured) != 0 {
		fmt.Fprintf(output, "  Configured T3 peers (%d)\n", len(configured))
	} else {
		fmt.Fprintln(output, "  Optional T3 peer linking", templateGuide+"#link-t3-servers")
	}
	return ready
}

func verifySetupTailscale(ctx context.Context, output io.Writer, configuredTags string, runtime runtimeConfig) (verified, needsConsole bool) {
	localOK := checkLocalTailscale(ctx, output)
	id, secret := runtime.value(tailClientID), runtime.value(tailSecret)
	if id == "" || secret == "" {
		return false, true
	}
	tags, err := peers.ParseEnrollmentTags(configuredTags)
	if err != nil {
		return false, false
	}
	if _, err := peers.VerifyEnrollment(ctx, id, secret, tags); err != nil {
		fmt.Fprintln(output, "  Needs attention", err)
		return false, errors.Is(err, peers.ErrEnrollmentAuthorization)
	}
	fmt.Fprintln(output, "  Verified Tailscale OAuth can issue a scoped token for", strings.Join(tags, ", "))
	return localOK, false
}

func repairTailscaleSetup(ctx context.Context, dryRun bool, output io.Writer) error {
	id, secret, err := tailscaleCredentials()
	if err != nil {
		return err
	}
	tags, err := workstationTailscaleTags()
	if err != nil {
		return err
	}
	_, err = peers.VerifyEnrollment(ctx, id, secret, tags)
	if errors.Is(err, peers.ErrEnrollmentTagDenied) {
		added, repairErr := peers.EnsureEnrollmentTags(ctx, id, secret, tags, dryRun)
		if repairErr != nil {
			return repairErr
		}
		if len(added) == 0 {
			return fmt.Errorf("the enrollment tags are already defined. Assign %s to the OAuth client in Trust credentials", strings.Join(tags, ", "))
		}
		if dryRun {
			fmt.Fprintln(output, "Would define Tailscale tags", strings.Join(added, ", "))
			return nil
		}
		fmt.Fprintln(output, "Defined Tailscale tags", strings.Join(added, ", "))
		_, err = peers.VerifyEnrollment(ctx, id, secret, tags)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "Verified Tailscale OAuth can issue a scoped token for", strings.Join(tags, ", "))
	return nil
}

func openTailscaleConsole(diagnostic io.Writer) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", tailscaleGuide)
	case "windows":
		cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", tailscaleGuide)
	default:
		cmd = exec.Command("xdg-open", tailscaleGuide)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(diagnostic, "Open Tailscale Trust credentials at", tailscaleGuide)
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintln(diagnostic, "Open Tailscale Trust credentials at", tailscaleGuide)
			return
		}
	case <-time.After(500 * time.Millisecond):
		// Some launchers stay alive with the browser they start.
	}
	fmt.Fprintln(diagnostic, "Opening Tailscale Trust credentials at", tailscaleGuide)
}

func checkLocalTailscale(ctx context.Context, output io.Writer) bool {
	path, err := exec.LookPath("tailscale")
	if err != nil {
		fmt.Fprintln(output, "  Local Tailscale status unavailable. Check that the T3 Code machine has joined the tailnet.")
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, path, "status", "--json", "--peers=false").Output()
	var status struct {
		BackendState   string
		CurrentTailnet struct{ MagicDNSEnabled bool }
		CertDomains    []string
	}
	if json.Unmarshal(data, &status) != nil || status.BackendState == "" {
		fmt.Fprintln(output, "  Needs attention local Tailscale status. Start the Tailscale client and retry.")
		return false
	}
	if status.BackendState != "Running" || err != nil {
		fmt.Fprintln(output, "  Needs attention local Tailscale connection. Sign in and connect to your tailnet.")
		return false
	}
	fmt.Fprintln(output, "  Verified local Tailscale connection")
	if status.CurrentTailnet.MagicDNSEnabled {
		fmt.Fprintln(output, "  Verified MagicDNS")
	} else {
		fmt.Fprintln(output, "  Needs attention MagicDNS. Enable it at", tailscaleDNS)
	}
	if len(status.CertDomains) > 0 {
		fmt.Fprintln(output, "  Verified Tailscale HTTPS certificates are enabled")
	} else {
		fmt.Fprintln(output, "  Needs attention Tailscale HTTPS certificates. Enable them at", tailscaleDNS)
	}
	return status.CurrentTailnet.MagicDNSEnabled && len(status.CertDomains) > 0
}
