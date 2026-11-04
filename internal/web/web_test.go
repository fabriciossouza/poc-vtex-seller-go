package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/fabriciossouza/poc-vtex-seller-go/api"
	"github.com/fabriciossouza/poc-vtex-seller-go/internal/web"
	"gopkg.in/yaml.v3"
)

func get(t *testing.T, path string) (int, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	web.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, rec.Header().Get("Content-Type"), string(body)
}

func TestPanelServesTheFourScenariosAndTheResetButton(t *testing.T) {
	status, ctype, body := get(t, "/")

	if status != 200 || !strings.HasPrefix(ctype, "text/html") {
		t.Fatalf("status = %d, content-type = %q", status, ctype)
	}
	for _, id := range []string{"btn-dup", "btn-down", "btn-up", "btn-replay", "btn-happy", "btn-reset"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("panel lacks button %s", id)
		}
	}
	if !strings.Contains(body, "simulador") {
		t.Error("the panel must say that the marketplace is a simulator")
	}
}

func TestServerTextEntersThePanelOnlyThroughTextContent(t *testing.T) {
	_, ctype, js := get(t, "/assets/app.js")

	if !strings.Contains(ctype, "javascript") {
		t.Fatalf("content-type = %q", ctype)
	}
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(js, banned) {
			t.Errorf("app.js uses %s", banned)
		}
	}
	if !strings.Contains(js, "textContent") {
		t.Error("app.js should render with textContent")
	}
}

func TestNoPageLoadsAnythingFromAnotherOrigin(t *testing.T) {
	external := regexp.MustCompile(`(?:src|href)\s*=\s*"(?:https?:)?//|url\(\s*['"]?(?:https?:)?//|import\s.*from\s+['"]https?:|fetch\(\s*['"]https?:`)
	for _, path := range []string{"/", "/docs/", "/assets/app.js", "/assets/style.css", "/docs/swagger-initializer.js"} {
		status, _, body := get(t, path)
		if status != 200 {
			t.Fatalf("%s: status %d", path, status)
		}
		if external.MatchString(body) {
			t.Errorf("%s references another origin", path)
		}
	}
}

func TestDocsServeVendoredSwaggerUIAndTheSpec(t *testing.T) {
	status, _, page := get(t, "/docs/")
	if status != 200 || !strings.Contains(page, `"/docs/swagger/swagger-ui-bundle.js"`) {
		t.Fatalf("docs page: status %d\n%s", status, page)
	}
	if status, _, _ := get(t, "/docs/swagger/swagger-ui-bundle.js"); status != 200 {
		t.Errorf("bundle status = %d", status)
	}
	if status, _, _ := get(t, "/docs/swagger/swagger-ui.css"); status != 200 {
		t.Errorf("css status = %d", status)
	}
	status, ctype, spec := get(t, "/docs/openapi.yaml")
	if status != 200 || !strings.Contains(ctype, "yaml") || !strings.HasPrefix(spec, "openapi: 3.") {
		t.Fatalf("spec: status %d, content-type %q", status, ctype)
	}
}

func TestSpecDescribesEveryEndpointOfTheServer(t *testing.T) {
	_, _, spec := get(t, "/docs/openapi.yaml")
	for _, path := range []string{
		"/pvt/orderForms/simulation:", "/pvt/orders:", "/pvt/orders/{sellerOrderId}/fulfill:", "/pvt/orders/{sellerOrderId}/cancel:",
		"/demo/orders:", "/demo/orders/{marketplaceOrderId}/fulfill:", "/demo/mkt/pvt/orders/{marketplaceOrderId}/invoice:",
		"/demo/state:", "/demo/erp:", "/demo/dlq/replay:", "/demo/reset:", "/healthz:", "/metrics:",
	} {
		if !strings.Contains(spec, "\n  "+path) {
			t.Errorf("spec lacks %s", path)
		}
	}
}

func TestVendoredSwaggerKeepsItsLicense(t *testing.T) {
	if status, _, body := get(t, "/docs/swagger/LICENSE"); status != 200 || !strings.Contains(body, "Apache License") {
		t.Fatalf("license: status %d", status)
	}
}

func TestSpecIsValidYAMLWithOpenAPI3AndTheMainPaths(t *testing.T) {
	var doc struct {
		OpenAPI string                    `yaml:"openapi"`
		Paths   map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(api.Spec, &doc); err != nil {
		t.Fatalf("spec is not valid YAML: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi = %q, want 3.x", doc.OpenAPI)
	}
	for path, method := range map[string]string{
		"/pvt/orderForms/simulation": "post", "/pvt/orders": "post",
		"/pvt/orders/{sellerOrderId}/fulfill": "post", "/demo/state": "get", "/healthz": "get",
	} {
		if _, ok := doc.Paths[path][method]; !ok {
			t.Errorf("spec lacks %s %s", method, path)
		}
	}
}

func TestSpecTextIsWrittenWithAccents(t *testing.T) {
	for _, word := range []string{"simulacao", "Simulacao", "nao ", "preco", "requisicoes", "Saude", "servico", "Metricas", "Operacao", "demonstracao", "superficie"} {
		if strings.Contains(string(api.Spec), word) {
			t.Errorf("spec has %q without accent", word)
		}
	}
}
