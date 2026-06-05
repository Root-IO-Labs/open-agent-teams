package products_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/internal/products"
)

// newTestServer returns a test HTTP server with the products handler registered.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	store := products.NewStore()
	h := products.NewHandler(store)
	h.RegisterRoutes(mux)
	return httptest.NewServer(mux)
}

// postJSON sends a POST request with a JSON body and returns the response.
func postJSON(t *testing.T, server *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(server.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// decodeBody decodes the JSON response body into v.
func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// --- Tests ---

func TestCreateProduct(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	resp := postJSON(t, srv, "/products", map[string]any{
		"name":        "Widget",
		"description": "A small widget",
		"price":       9.99,
		"stock":       100,
	})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var p products.Product
	decodeBody(t, resp, &p)

	if p.ID == "" {
		t.Error("expected non-empty ID")
	}
	if p.Name != "Widget" {
		t.Errorf("expected name 'Widget', got %q", p.Name)
	}
	if p.Price != 9.99 {
		t.Errorf("expected price 9.99, got %f", p.Price)
	}
	if p.Stock != 100 {
		t.Errorf("expected stock 100, got %d", p.Stock)
	}
}

func TestCreateProduct_Validation(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	tests := []struct {
		name string
		body map[string]any
	}{
		{"missing name", map[string]any{"price": 1.0, "stock": 0}},
		{"negative price", map[string]any{"name": "X", "price": -1.0}},
		{"negative stock", map[string]any{"name": "X", "price": 1.0, "stock": -5}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := postJSON(t, srv, "/products", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

func TestGetProduct(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Create a product first.
	createResp := postJSON(t, srv, "/products", map[string]any{
		"name":  "Gadget",
		"price": 19.99,
		"stock": 50,
	})
	var created products.Product
	decodeBody(t, createResp, &created)

	// Fetch it.
	resp, err := http.Get(srv.URL + "/products/" + created.ID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var p products.Product
	decodeBody(t, resp, &p)
	if p.ID != created.ID {
		t.Errorf("expected ID %q, got %q", created.ID, p.ID)
	}
}

func TestGetProduct_NotFound(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/products/nonexistent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestListProducts(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Create two products.
	for _, name := range []string{"Alpha", "Beta"} {
		r := postJSON(t, srv, "/products", map[string]any{"name": name, "price": 1.0, "stock": 1})
		r.Body.Close()
	}

	resp, err := http.Get(srv.URL + "/products")
	if err != nil {
		t.Fatalf("GET /products: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body struct {
		Products []products.Product `json:"products"`
		Total    int                `json:"total"`
	}
	decodeBody(t, resp, &body)

	if body.Total != 2 {
		t.Errorf("expected total 2, got %d", body.Total)
	}
	if len(body.Products) != 2 {
		t.Errorf("expected 2 products, got %d", len(body.Products))
	}
}

func TestUpdateProduct(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Create a product.
	createResp := postJSON(t, srv, "/products", map[string]any{
		"name":  "Old Name",
		"price": 5.0,
		"stock": 10,
	})
	var created products.Product
	decodeBody(t, createResp, &created)

	// Update it.
	newPrice := 12.50
	body, _ := json.Marshal(map[string]any{"price": newPrice})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/products/"+created.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var updated products.Product
	decodeBody(t, resp, &updated)
	if updated.Price != newPrice {
		t.Errorf("expected price %f, got %f", newPrice, updated.Price)
	}
	if updated.Name != created.Name {
		t.Errorf("name should not change: got %q", updated.Name)
	}
}

func TestDeleteProduct(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// Create a product.
	createResp := postJSON(t, srv, "/products", map[string]any{
		"name":  "Doomed",
		"price": 1.0,
		"stock": 1,
	})
	var created products.Product
	decodeBody(t, createResp, &created)

	// Delete it.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/products/"+created.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	// Confirm it is gone.
	getResp, err := http.Get(srv.URL + "/products/" + created.ID)
	if err != nil {
		t.Fatalf("GET after delete: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", getResp.StatusCode)
	}
}

func TestDeleteProduct_NotFound(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/products/ghost", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/products", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}
