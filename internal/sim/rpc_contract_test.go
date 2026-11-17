package sim_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func rpcRequest(service, operation string, security, fields map[string]any) []byte {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "client-request", "method": "invoke", "params": map[string]any{"serviceId": service, "operationId": operation, "ctx": map[string]any{"securityCtx": security}, "input": rpcStruct("operation-input", fields)}})
	return body
}
func rpcStruct(name string, fields map[string]any) any {
	return map[string]any{"STRUCTURE": map[string]any{name: fields}}
}
func rpcResult(t *testing.T, srv *httptest.Server, body []byte) map[string]any {
	t.Helper()
	res, err := http.Post(srv.URL+"/api", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var response map[string]any
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || response["jsonrpc"] != "2.0" || response["id"] != "client-request" {
		t.Fatal("invalid vAPI envelope")
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatal("missing vAPI result")
	}
	return result
}
func requireRPCOutput(t *testing.T, result map[string]any) any {
	t.Helper()
	value, ok := result["output"]
	if !ok {
		t.Fatalf("vAPI request failed (output missing)")
	}
	return value
}
func structFields(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	v, ok := value.(map[string]any)
	if !ok {
		t.Fatal("missing typed structure")
	}
	s, ok := v["STRUCTURE"].(map[string]any)
	if !ok {
		s, ok = v["ERROR"].(map[string]any)
	}
	if !ok {
		t.Fatal("missing STRUCTURE")
	}
	fields, ok := s[name].(map[string]any)
	if !ok {
		t.Fatalf("missing required structure %s", name)
	}
	return fields
}

// Oracle: TagServiceTagHandler calls session.create, tag.list and session.delete.
// The Java client requires SECRET at create and a list (including empty) at list.
func TestOperationsVapiTagFlow(t *testing.T) {
	e := start(t, "ops-vapi")
	security := map[string]any{"schemeId": "com.vmware.vapi.std.security.user_pass", "userName": "user", "password": "pass"}
	output := requireRPCOutput(t, rpcResult(t, e.srv, rpcRequest("com.vmware.cis.session", "create", security, nil)))
	token, ok := output.(map[string]any)["SECRET"].(string)
	if !ok || token == "" {
		t.Fatal("session lacks required SECRET")
	}
	security = map[string]any{"schemeId": "com.vmware.vapi.std.security.session_id", "sessionId": token}
	output = requireRPCOutput(t, rpcResult(t, e.srv, rpcRequest("com.vmware.cis.tagging.tag", "list", security, nil)))
	tags, ok := output.([]any)
	if !ok || len(tags) != 0 {
		t.Fatal("tag list must be an empty array")
	}
	requireRPCOutput(t, rpcResult(t, e.srv, rpcRequest("com.vmware.cis.session", "delete", security, nil)))
	result := rpcResult(t, e.srv, rpcRequest("com.vmware.cis.tagging.tag", "list", security, nil))
	fields := structFields(t, result["error"], "com.vmware.vapi.std.errors.unauthenticated")
	if fields["messages"] == nil {
		t.Fatal("refusal lacks required messages")
	}
}

// Oracle: VcAuthentication acquires at the root STS route; the SAML client
// requires a UPN and a valid signature. A changed subject must not authenticate.
func TestOperationsRootSTSAndBearerSession(t *testing.T) {
	sim, srv, _ := signOnSim(t, nil)
	now := time.Now().UTC()
	res, body := postTo(t, srv, "/sts/STSService", stsAction, tokenRequest("operator", "s3cret", now, now.Add(time.Hour)))
	if res.StatusCode != 200 {
		t.Fatal("root token endpoint refused valid credentials")
	}
	var envelope struct {
		Body struct {
			Collection struct {
				Response struct {
					Token struct {
						Assertion string `xml:",innerxml"`
					} `xml:"RequestedSecurityToken"`
				} `xml:"RequestSecurityTokenResponse"`
			} `xml:"RequestSecurityTokenResponseCollection"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	assertion := strings.TrimSpace(envelope.Body.Collection.Response.Token.Assertion)
	if assertion == "" {
		t.Fatal("missing required assertion")
	}
	acquire := func(token string) map[string]any {
		return rpcResult(t, srv, rpcRequest("com.vmware.cis.session", "create", map[string]any{"schemeId": "com.vmware.vapi.std.security.saml_bearer_token", "samlToken": token}, nil))
	}
	login := requireRPCOutput(t, acquire(assertion)).(map[string]any)
	security := map[string]any{"sessionId": login["SECRET"]}
	scope := "HostSystem:" + sim.Clusters[0].Hosts[0].Value + ",Host.Stats.Query"
	spec := rpcStruct("com.vmware.vcenter.authentication.token.issue_spec", map[string]any{"grant_type": "urn:ietf:params:oauth:grant-type:token-exchange", "audience": map[string]any{"OPTIONAL": "vmware-tes:esx:auth"}, "scope": map[string]any{"OPTIONAL": scope}, "requested_token_type": map[string]any{"OPTIONAL": "urn:ietf:params:oauth:token-type:id_token"}, "subject_token": map[string]any{"OPTIONAL": base64.StdEncoding.EncodeToString([]byte(assertion))}, "subject_token_type": map[string]any{"OPTIONAL": "urn:ietf:params:oauth:token-type:saml2"}})
	exchanged := rpcResult(t, srv, rpcRequest("com.vmware.vcenter.authentication.token", "issue", security, map[string]any{"spec": spec}))
	info := structFields(t, requireRPCOutput(t, exchanged), "com.vmware.oauth2.token_info")
	access, ok := info["access_token"].(string)
	if !ok || access == "" || info["token_type"] != "Bearer" || info["expires_in"] == nil || info["scope"] == nil || info["issued_token_type"] == nil {
		t.Fatal("missing token exchange fields")
	}
	parts := strings.Split(access, ".")
	if len(parts) != 3 {
		t.Fatal("access token is not a signed JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil || claims["scope"] != scope || claims["sub"] != "operator" || claims["iss"] != "vcenter-api-simulator" {
		t.Fatal("incorrect simulator token claims")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	certificate := issuerCertificate(t, srv)
	if rsa.VerifyPKCS1v15(certificate.PublicKey.(*rsa.PublicKey), crypto.SHA256, digest[:], signature) != nil {
		t.Fatal("exchange token is not signed by the simulator")
	}

	for _, bad := range []string{strings.ReplaceAll(assertion, "operator@vsphere.local", "intruder@vsphere.local"), strings.ReplaceAll(assertion, "LicenseService.Administrators", "Administrators"), strings.ReplaceAll(assertion, ">false</saml2:AttributeValue>", ">true</saml2:AttributeValue>")} {
		if acquire(bad)["error"] == nil {
			t.Fatal("tampered assertion accepted")
		}
	}
	if err := sim.UpdateCredentials(func() ([]string, error) { return []string{"operator"}, nil }); err != nil {
		t.Fatal(err)
	}
	if acquire(assertion)["error"] == nil {
		t.Fatal("credential update did not revoke bearer token")
	}
}

// Oracle: QuerySpecManager and StreamManager require create_result.query and
// messages, then result.query/timestamp/snapshots/messages on every vAPI frame.
// No GPU is configured, so an empty snapshot is the truthful sample.
func TestOperationsVstatsQueryAndStream(t *testing.T) {
	e := start(t, "ops-stats")
	out := requireRPCOutput(t, rpcResult(t, e.srv, rpcRequest("com.vmware.cis.session", "create", map[string]any{"userName": "user", "password": "pass"}, nil)))
	security := map[string]any{"sessionId": out.(map[string]any)["SECRET"]}
	spec := rpcStruct("com.vmware.stats.provider.queries.create_spec", map[string]any{"view": map[string]any{"OPTIONAL": "vstats"}, "time_to_live": map[string]any{"OPTIONAL": 600}, "instances": []any{rpcStruct("com.vmware.stats.provider.counter_instance", map[string]any{"counter": map[string]any{"OPTIONAL": "gpu.compute.clock.used.DEVICE"}, "resource_ids": map[string]any{}})}})
	create := rpcResult(t, e.srv, rpcRequest("com.vmware.stats.provider.queries", "create", security, map[string]any{"spec": spec}))
	fields := structFields(t, requireRPCOutput(t, create), "com.vmware.stats.provider.queries.create_result")
	qid, ok := fields["query"].(string)
	if !ok || qid == "" || fields["messages"] == nil {
		t.Fatal("missing required query result fields")
	}
	requireRPCOutput(t, rpcResult(t, e.srv, rpcRequest("com.vmware.stats.provider.stream", "create", security, map[string]any{"query": qid, "spec": rpcStruct("com.vmware.stats.provider.stream.create_spec", map[string]any{"interval": map[string]any{"OPTIONAL": 1}})})))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/stats", bytes.NewReader(rpcRequest("com.vmware.stats.provider.stream", "attach", security, map[string]any{"query": qid, "spec": rpcStruct("com.vmware.stats.provider.stream.attach_spec", map[string]any{"client_id": "test-client"})})))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "application/vnd.vmware.vapi.stream.json" {
		t.Fatal("wrong streaming content type")
	}
	reader := bufio.NewReader(response.Body)
	for i := 0; i < 2; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 32)
		if err != nil || size < 1 || size > 1<<20 {
			t.Fatal("invalid vAPI frame length")
		}
		frame := make([]byte, size)
		if _, err := io.ReadFull(reader, frame); err != nil {
			t.Fatal(err)
		}
		if trailer, err := reader.ReadString('\n'); err != nil || trailer != "\r\n" {
			t.Fatal("invalid frame trailer")
		}
		var result struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(frame, &result); err != nil {
			t.Fatal(err)
		}
		sample := structFields(t, requireRPCOutput(t, result.Result), "com.vmware.stats.provider.result")
		if sample["query"] != qid || sample["timestamp"] == nil || sample["snapshots"] == nil || sample["messages"] == nil {
			t.Fatal("required stream fields absent")
		}
		if _, err := time.Parse(time.RFC3339Nano, sample["timestamp"].(string)); err != nil {
			t.Fatal("invalid sample timestamp")
		}
	}
	cancel()
	response.Body.Close()
	for _, service := range []string{"com.vmware.stats.provider.stream", "com.vmware.stats.provider.queries"} {
		requireRPCOutput(t, rpcResult(t, e.srv, rpcRequest(service, "delete", security, map[string]any{"query": qid})))
	}
	missing := rpcResult(t, e.srv, rpcRequest("com.vmware.stats.provider.stream", "create", security, map[string]any{"query": qid, "spec": rpcStruct("com.vmware.stats.provider.stream.create_spec", nil)}))
	if missing["error"] == nil {
		t.Fatal("deleted query was reusable")
	}
}
