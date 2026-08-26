// Command agentctl is the developer and operator CLI for AgentGate.
//
// It is the same set of calls a CI pipeline makes, in a form an engineer can
// run by hand. That is deliberate: a platform whose automation path and whose
// human path are different implementations will eventually disagree, and the
// disagreement will surface during an incident.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/registry"
	"github.com/agentgate/agentgate/internal/version"
	"github.com/agentgate/agentgate/internal/yamlite"
)

const usage = `agentctl — AgentGate control CLI

Usage:
  agentctl <command> [flags]

Commands:
  register     Register an agent version from a manifest file
  token        Obtain an access token (client credentials or token exchange)
  agents       List registered agents
  agent        Show one agent
  promote      Request a promotion to staging or production
  approve      Record an approval or rejection on a promotion
  promotions   List promotion requests
  quarantine   Disable an agent version immediately
  waiver       Grant a time-boxed promotion gate exception
  models       List the models a token is entitled to
  chat         Send a chat completion through the gateway
  fleet        Show the fleet view
  slo          Show service level objectives and error budgets
  chargeback   Show the chargeback report
  version      Print the version

Environment:
  AGENTGATE_CONTROLPLANE_URL   default http://localhost:8081
  AGENTGATE_GATEWAY_URL        default http://localhost:8080
  AGENTGATE_FLEET_URL          default http://localhost:8082
  AGENTGATE_TOKEN              bearer token for authenticated calls
  AGENTGATE_ACTOR              actor recorded in audit records
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "register":
		err = cmdRegister(args)
	case "token":
		err = cmdToken(args)
	case "agents":
		err = cmdAgents(args)
	case "agent":
		err = cmdAgent(args)
	case "promote":
		err = cmdPromote(args)
	case "approve":
		err = cmdApprove(args)
	case "promotions":
		err = cmdPromotions(args)
	case "quarantine":
		err = cmdQuarantine(args)
	case "waiver":
		err = cmdWaiver(args)
	case "models":
		err = cmdModels(args)
	case "chat":
		err = cmdChat(args)
	case "fleet":
		err = cmdFleet(args)
	case "slo":
		err = cmdSLO(args)
	case "chargeback":
		err = cmdChargeback(args)
	case "version", "--version", "-v":
		fmt.Println(version.String())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func controlPlane() string { return envOr("AGENTGATE_CONTROLPLANE_URL", "http://localhost:8081") }
func gatewayURL() string   { return envOr("AGENTGATE_GATEWAY_URL", "http://localhost:8080") }
func fleetURL() string     { return envOr("AGENTGATE_FLEET_URL", "http://localhost:8082") }

func client() *http.Client { return &http.Client{Timeout: 5 * time.Minute} }

// call performs a JSON request and decodes the response, surfacing
// problem+json errors in the form the API actually returns them so that a
// failure in CI is self-explanatory.
func call(method, url string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := os.Getenv("AGENTGATE_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if actor := os.Getenv("AGENTGATE_ACTOR"); actor != "" {
		req.Header.Set("x-agentgate-actor", actor)
	}
	resp, err := client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var p httpx.Problem
		if json.Unmarshal(raw, &p) == nil && p.Code != "" {
			return fmt.Errorf("%s (%s): %s", p.Title, p.Code, p.Detail)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	fmt.Println(indent(raw))
	return nil
}

func indent(raw []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") != nil {
		return string(raw)
	}
	return buf.String()
}

func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	manifest := fs.String("f", "agent.yaml", "manifest file (JSON, or the YAML subset)")
	env := fs.String("env", "dev", "environment to register into")
	ver := fs.String("version", "", "agent version; overrides the manifest")
	commit := fs.String("commit", "", "commit SHA")
	image := fs.String("image", "", "container image reference")
	credential := fs.Bool("credential", false, "issue a client secret; prefer federated workload identity")
	_ = fs.Parse(args)

	raw, err := os.ReadFile(*manifest)
	if err != nil {
		return err
	}
	var req registry.RegistrationRequest
	if err := unmarshalManifest(raw, &req); err != nil {
		return err
	}
	if *ver != "" {
		req.Version = *ver
	}
	if *commit != "" {
		req.CommitSHA = *commit
	}
	if *image != "" {
		req.Image = *image
	}
	if req.Env == "" {
		req.Env = identity.Environment(*env)
	}

	payload := map[string]any{}
	blob, _ := json.Marshal(req)
	_ = json.Unmarshal(blob, &payload)
	payload["issue_credential"] = *credential

	var res registry.RegistrationResult
	if err := call(http.MethodPost, controlPlane()+"/api/v1/agents", payload, &res); err != nil {
		return err
	}
	fmt.Printf("registered %s version %s in %s (state %s, agent_id %s)\n",
		res.Agent.IdentityS, res.Version, res.Env, res.State, res.Agent.AgentID)
	for _, w := range res.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
	if res.ClientSecret != "" {
		fmt.Printf("\nclient_id:     %s\nclient_secret: %s\n", res.ClientID, res.ClientSecret)
		fmt.Println("This secret is shown once. Store it in the vault now.")
	}
	return nil
}

func cmdToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	clientID := fs.String("client-id", "", "client id for the client_credentials grant")
	secret := fs.String("client-secret", "", "client secret")
	subject := fs.String("subject-token", "", "platform token for the token-exchange grant")
	agentIdentity := fs.String("agent", "", "agent identity URI for the token-exchange grant")
	env := fs.String("env", "dev", "environment")
	agentVersion := fs.String("agent-version", "", "specific version; defaults to the active one")
	quiet := fs.Bool("q", false, "print only the access token")
	_ = fs.Parse(args)

	form := url.Values{}
	form.Set("env", *env)
	if *agentVersion != "" {
		form.Set("agent_version", *agentVersion)
	}
	switch {
	case *subject != "":
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
		form.Set("subject_token", *subject)
		form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")
		form.Set("agent_identity", *agentIdentity)
	case *clientID != "":
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", *clientID)
		form.Set("client_secret", *secret)
	default:
		return fmt.Errorf("either -client-id or -subject-token is required")
	}

	resp, err := client().PostForm(controlPlane()+"/oauth2/token", form)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		AgentID     string `json:"agent_id"`
		Env         string `json:"env"`
		Version     string `json:"agent_version"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if *quiet {
		fmt.Println(out.AccessToken)
		return nil
	}
	fmt.Printf("agent_id: %s\nenv:      %s\nversion:  %s\nexpires:  %ds\n\nexport AGENTGATE_TOKEN=%s\n",
		out.AgentID, out.Env, out.Version, out.ExpiresIn, out.AccessToken)
	return nil
}

func cmdAgents(args []string) error {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	team := fs.String("team", "", "filter by team")
	env := fs.String("env", "", "filter by environment")
	_ = fs.Parse(args)
	q := url.Values{}
	if *team != "" {
		q.Set("team", *team)
	}
	if *env != "" {
		q.Set("env", *env)
	}
	var out struct {
		Agents []registry.Agent `json:"agents"`
	}
	if err := call(http.MethodGet, controlPlane()+"/api/v1/agents?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	w := bufio.NewWriter(os.Stdout)
	defer func() { _ = w.Flush() }()
	fmt.Fprintf(w, "%-46s %-16s %-22s %-10s %s\n", "IDENTITY", "TEAM", "OWNER", "CLASS", "VERSIONS")
	for _, a := range out.Agents {
		var vers []string
		for _, v := range a.Versions {
			vers = append(vers, fmt.Sprintf("%s/%s=%s", v.Env, v.Version, v.State))
		}
		fmt.Fprintf(w, "%-46s %-16s %-22s %-10s %s\n",
			a.IdentityS, a.Owner.Team, a.Owner.OnCall, a.DataClassification, strings.Join(vers, " "))
	}
	return nil
}

func cmdAgent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agentctl agent <agent-id>")
	}
	return call(http.MethodGet, controlPlane()+"/api/v1/agents/"+args[0], nil, nil)
}

func cmdPromote(args []string) error {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	agentID := fs.String("agent-id", "", "agent id")
	ver := fs.String("version", "", "version to promote")
	to := fs.String("to", "staging", "target environment")
	_ = fs.Parse(args)
	if *agentID == "" || *ver == "" {
		return fmt.Errorf("-agent-id and -version are required")
	}
	var p registry.PromotionRequest
	err := call(http.MethodPost, controlPlane()+"/api/v1/promotions",
		map[string]any{"agent_id": *agentID, "version": *ver, "to": *to}, &p)
	if err != nil && p.ID == "" {
		return err
	}
	fmt.Printf("promotion %s: %s → %s, state %s\n", p.ID, p.From, p.To, p.State)
	fmt.Println("\ngate:")
	for _, c := range p.Gate.Checks {
		mark := "ok  "
		switch c.Status {
		case registry.CheckFail:
			mark = "FAIL"
		case registry.CheckWaived:
			mark = "waiv"
		case registry.CheckSkip:
			mark = "skip"
		}
		fmt.Printf("  [%s] %-24s %s\n", mark, c.Name, c.Detail)
		if c.Observed != "" {
			fmt.Printf("         observed %s, required %s\n", c.Observed, c.Required)
		}
	}
	if p.State == registry.PromotionPending {
		fmt.Printf("\nawaiting %d approvals; one owning-team and one platform approver, neither the requester\n",
			registry.RequiredApprovals(p.To))
		if p.ChangeRef != "" {
			fmt.Printf("change record: %s\n", p.ChangeRef)
		}
	}
	if p.State == registry.PromotionBlocked {
		return fmt.Errorf("promotion blocked by gate: %s", strings.Join(p.Gate.Failed(), ", "))
	}
	return nil
}

func cmdApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	id := fs.String("id", "", "promotion id")
	role := fs.String("role", "owning_team", "owning_team or platform")
	reject := fs.Bool("reject", false, "record a rejection instead of an approval")
	comment := fs.String("comment", "", "comment recorded in the audit trail")
	_ = fs.Parse(args)
	if *id == "" {
		return fmt.Errorf("-id is required")
	}
	var out struct {
		Error     string `json:"error"`
		State     string `json:"state"`
		Approvals []struct {
			Actor    string `json:"actor"`
			Role     string `json:"role"`
			Approved bool   `json:"approved"`
		} `json:"approvals"`
		Promotion *struct {
			State string `json:"state"`
		} `json:"promotion"`
	}
	err := call(http.MethodPost, controlPlane()+"/api/v1/promotions/"+*id+"/approve",
		map[string]any{"role": *role, "approved": !*reject, "comment": *comment}, &out)
	if err != nil {
		return err
	}
	// The two-party rules produce refusals that are decisions rather than
	// faults; a one-line answer is more useful to a human than the whole
	// promotion document.
	if out.Error != "" {
		state := out.State
		if out.Promotion != nil {
			state = out.Promotion.State
		}
		return fmt.Errorf("%s (promotion is %s)", out.Error, state)
	}
	fmt.Printf("promotion %s is now %s\n", *id, out.State)
	for _, a := range out.Approvals {
		verdict := "approved"
		if !a.Approved {
			verdict = "rejected"
		}
		fmt.Printf("  %-28s %-12s %s\n", a.Actor, a.Role, verdict)
	}
	if out.State == "pending" {
		fmt.Println("  still waiting: production needs one owning-team and one platform approver, neither the requester")
	}
	return nil
}

func cmdPromotions(args []string) error {
	fs := flag.NewFlagSet("promotions", flag.ExitOnError)
	state := fs.String("state", "", "filter by state")
	agentID := fs.String("agent-id", "", "filter by agent")
	_ = fs.Parse(args)
	q := url.Values{}
	if *state != "" {
		q.Set("state", *state)
	}
	if *agentID != "" {
		q.Set("agent_id", *agentID)
	}
	return call(http.MethodGet, controlPlane()+"/api/v1/promotions?"+q.Encode(), nil, nil)
}

func cmdQuarantine(args []string) error {
	fs := flag.NewFlagSet("quarantine", flag.ExitOnError)
	agentID := fs.String("agent-id", "", "agent id")
	ver := fs.String("version", "", "version")
	env := fs.String("env", "prod", "environment")
	reason := fs.String("reason", "", "why; recorded in the audit trail")
	_ = fs.Parse(args)
	if *agentID == "" || *ver == "" || *reason == "" {
		return fmt.Errorf("-agent-id, -version and -reason are required")
	}
	return call(http.MethodPost, controlPlane()+"/api/v1/agents/"+*agentID+"/quarantine",
		map[string]any{"version": *ver, "env": *env, "reason": *reason}, nil)
}

func cmdWaiver(args []string) error {
	fs := flag.NewFlagSet("waiver", flag.ExitOnError)
	agentID := fs.String("agent-id", "", "agent id")
	gate := fs.String("gate", "", "gate name to waive")
	reason := fs.String("reason", "", "why")
	days := fs.Int("days", 30, "expiry in days, at most 90")
	_ = fs.Parse(args)
	if *agentID == "" || *gate == "" || *reason == "" {
		return fmt.Errorf("-agent-id, -gate and -reason are required")
	}
	return call(http.MethodPost, controlPlane()+"/api/v1/waivers",
		map[string]any{"agent_id": *agentID, "gate": *gate, "reason": *reason, "days": *days}, nil)
}

func cmdModels(args []string) error {
	_ = args
	return call(http.MethodGet, gatewayURL()+"/v1/models", nil, nil)
}

func cmdChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	model := fs.String("model", "general-chat", "logical model")
	prompt := fs.String("p", "", "prompt; reads stdin when empty")
	stream := fs.Bool("stream", false, "stream the response")
	maxTokens := fs.Int("max-tokens", 256, "max output tokens")
	temp := fs.Float64("temperature", 0, "sampling temperature")
	_ = fs.Parse(args)

	text := *prompt
	if text == "" {
		raw, _ := io.ReadAll(os.Stdin)
		text = strings.TrimSpace(string(raw))
	}
	if text == "" {
		return fmt.Errorf("a prompt is required, via -p or stdin")
	}
	body := map[string]any{
		"model":       *model,
		"messages":    []map[string]string{{"role": "user", "content": text}},
		"max_tokens":  *maxTokens,
		"temperature": *temp,
		"stream":      *stream,
	}
	if *stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, gatewayURL()+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("AGENTGATE_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// The response headers are half the value of the gateway; printing them
	// makes routing, cost and cache behaviour visible without a dashboard.
	fmt.Fprintf(os.Stderr, "request %s trace %s provider %s model %s attempts %s cache %s cost %s\n",
		resp.Header.Get("x-agentgate-request-id"), resp.Header.Get("x-agentgate-trace-id"),
		resp.Header.Get("x-agentgate-provider"), resp.Header.Get("x-agentgate-model"),
		resp.Header.Get("x-agentgate-attempts"), resp.Header.Get("x-agentgate-cache"),
		resp.Header.Get("x-agentgate-cost-usd"))

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if !*stream {
		body, _ := io.ReadAll(resp.Body)
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &out) == nil && len(out.Choices) > 0 {
			fmt.Println(out.Choices[0].Message.Content)
			return nil
		}
		fmt.Println(indent(body))
		return nil
	}
	return httpx.ReadSSE(resp.Body, func(ev httpx.SSEEvent) error {
		if ev.IsDone() {
			fmt.Println()
			return nil
		}
		if ev.Event != "" {
			fmt.Fprintf(os.Stderr, "\n[%s] %s\n", ev.Event, ev.Data)
			return nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(ev.Data), &chunk) == nil && len(chunk.Choices) > 0 {
			fmt.Print(chunk.Choices[0].Delta.Content)
		}
		return nil
	})
}

func cmdFleet(args []string) error {
	_ = args
	return call(http.MethodGet, fleetURL()+"/api/v1/agents", nil, nil)
}

func cmdSLO(args []string) error {
	_ = args
	return call(http.MethodGet, fleetURL()+"/api/v1/slo", nil, nil)
}

func cmdChargeback(args []string) error {
	fs := flag.NewFlagSet("chargeback", flag.ExitOnError)
	period := fs.String("period", "day", "hour or day")
	costCenter := fs.String("cost-center", "", "filter by cost centre")
	_ = fs.Parse(args)
	q := url.Values{"period": {*period}}
	if *costCenter != "" {
		q.Set("cost_center", *costCenter)
	}
	return call(http.MethodGet, fleetURL()+"/api/v1/chargeback?"+q.Encode(), nil, nil)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// unmarshalManifest accepts either JSON or the YAML subset the platform uses
// for configuration, so a team can keep its agent manifest in whichever format
// the rest of its repository uses.
func unmarshalManifest(raw []byte, v any) error {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		return json.Unmarshal(raw, v)
	}
	return yamlite.Unmarshal(raw, v)
}
