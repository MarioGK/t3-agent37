package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/MarioGK/t3-agent37/internal/peers"
	"github.com/charmbracelet/huh"
)

const (
	templateGuide    = "https://github.com/MarioGK/t3-agent37/blob/main/docs/terrascale.md"
	tailscaleGuide   = "https://console.tailscale.com/admin/settings/trust-credentials"
	agent37Dashboard = "https://www.agent37.com/dashboard/cloud"
)

func setupWorkstation(ctx context.Context, check bool, output, diagnostic io.Writer) error {
	if !check && !terminalAvailable(diagnostic) {
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
	if key == "" {
		key = runtime.value(agent37KeyName)
	}
	if check {
		if !setupStatus(output, config.values, runtime, key) {
			return errors.New("workstation setup is incomplete")
		}
		return nil
	}

	values := &config.values
	usesCLIProxy := func() bool { return sandboxProvider(values.Provider, values.Template) == "cliproxy" }
	var apiKey, clientID, clientSecret, baseURL, clientKey string
	var save bool
	form := huh.NewForm(
		credentialInput("Agent37 API key", "Create a workspace API key at "+agent37Dashboard, &apiKey).
			WithHide(key != ""),
		sandboxDefaults(values, "Enter a template with Tailscale installed. Add @revision to pin it.\nTo publish the TerraScale template, see "+templateGuide),
		providerOptions(values),
		huh.NewGroup(huh.NewNote().Title("Private Tailscale access").
			Description("Define tag:agent37 and allow your T3 clients to reach it on port 443.\n"+
				"Enable MagicDNS and HTTPS certificates in the tailnet DNS settings.\n"+
				"Create an OAuth client with Auth Keys Write scoped to tag:agent37.\n"+tailscaleGuide).
			Next(true),
		).WithHide(runtime.value(tailClientID) != "" && runtime.value(tailSecret) != ""),
		credentialInput("Tailscale OAuth client ID", "Use the client ID from Trust credentials.", &clientID).
			WithHide(runtime.value(tailClientID) != ""),
		credentialInput("Tailscale OAuth client secret", "Use the secret from the same OAuth client.", &clientSecret).
			WithHide(runtime.value(tailSecret) != ""),
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
	).WithInput(os.Stdin).WithOutput(diagnostic)
	err = form.RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) || (err == nil && !save) {
		fmt.Fprintln(diagnostic, "Setup canceled.")
		return nil
	}
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
		if key == "" {
			key = runtime.value(agent37KeyName)
		}
	}
	values.Template = strings.TrimSpace(values.Template)
	values.Tailscale = nil
	if err := config.save(ctx); err != nil {
		return err
	}
	fmt.Fprintln(output, "Saved sandbox defaults to", config.path)
	setupStatus(output, *values, runtime, key)
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
	report(runtime.value(tailClientID) != "" && runtime.value(tailSecret) != "", "Tailscale OAuth client",
		"Create a client with Auth Keys Write for tag:agent37. "+tailscaleGuide)
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
	if ready {
		fmt.Fprintln(output, "Run t3-agent37 up to provision a sandbox and verify access.")
	}
	return ready
}
