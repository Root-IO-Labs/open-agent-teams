// Package products provides an in-memory REST API for managing products.
package products

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Product represents a product in the catalog.
type Product struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Price       float64   `json:"price"`
	Stock       int       `json:"stock"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CreateProductRequest holds the fields for creating a product.
type CreateProductRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
	Stock       int     `json:"stock"`
}

// UpdateProductRequest holds the fields for updating a product.
type UpdateProductRequest struct {
	Name        *string  `json:"name,omitempty"`
	Description *string  `json:"description,omitempty"`
	Price       *float64 `json:"price,omitempty"`
	Stock       *int     `json:"stock,omitempty"`
}

// Store is an in-memory product store protected by a mutex.
type Store struct {
	mu       sync.RWMutex
	products map[string]*Product
}

// NewStore returns an initialized product store.
func NewStore() *Store {
	return &Store{
		products: make(map[string]*Product),
	}
}

// Handler is the HTTP handler for the products API.
type Handler struct {
	store *Store
}

// NewHandler returns a Handler backed by the given store.
func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

// RegisterRoutes registers all product routes on mux.
// Routes registered:
//
//	GET    /products          – list all products
//	POST   /products          – create a product
//	GET    /products/{id}     – get a single product
//	PUT    /products/{id}     – replace a product
//	DELETE /products/{id}     – delete a product
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/products", h.handleCollection)
	mux.HandleFunc("/products/", h.handleItem)
}

// handleCollection routes GET /products and POST /products.
func (h *Handler) handleCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listProducts(w, r)
	case http.MethodPost:
		h.createProduct(w, r)
	default:
		methodNotAllowed(w)
	}
}

// handleItem routes /products/{id} requests.
func (h *Handler) handleItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/products/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getProduct(w, r, id)
	case http.MethodPut:
		h.updateProduct(w, r, id)
	case http.MethodDelete:
		h.deleteProduct(w, r, id)
	default:
		methodNotAllowed(w)
	}
}

// listProducts handles GET /products.
// Optional query params: limit (int), offset (int).
func (h *Handler) listProducts(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r)

	h.store.mu.RLock()
	all := make([]*Product, 0, len(h.store.products))
	for _, p := range h.store.products {
		all = append(all, p)
	}
	h.store.mu.RUnlock()

	// Apply pagination.
	total := len(all)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total || limit <= 0 {
		end = total
	}
	page := all[offset:end]

	writeJSON(w, http.StatusOK, map[string]any{
		"products": page,
		"total":    total,
	})
}

// createProduct handles POST /products.
func (h *Handler) createProduct(w http.ResponseWriter, r *http.Request) {
	var req CreateProductRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, "invalid JSON body")
		return
	}
	if err := validateCreate(req); err != nil {
		badRequest(w, err.Error())
		return
	}

	now := time.Now().UTC()
	p := &Product{
		ID:          uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		Price:       req.Price,
		Stock:       req.Stock,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	h.store.mu.Lock()
	h.store.products[p.ID] = p
	h.store.mu.Unlock()

	writeJSON(w, http.StatusCreated, p)
}

// getProduct handles GET /products/{id}.
func (h *Handler) getProduct(w http.ResponseWriter, _ *http.Request, id string) {
	h.store.mu.RLock()
	p, ok := h.store.products[id]
	h.store.mu.RUnlock()

	if !ok {
		notFound(w, id)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// updateProduct handles PUT /products/{id}.
func (h *Handler) updateProduct(w http.ResponseWriter, r *http.Request, id string) {
	var req UpdateProductRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, "invalid JSON body")
		return
	}

	h.store.mu.Lock()
	p, ok := h.store.products[id]
	if !ok {
		h.store.mu.Unlock()
		notFound(w, id)
		return
	}

	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.Description != nil {
		p.Description = *req.Description
	}
	if req.Price != nil {
		if *req.Price < 0 {
			h.store.mu.Unlock()
			badRequest(w, "price must be non-negative")
			return
		}
		p.Price = *req.Price
	}
	if req.Stock != nil {
		if *req.Stock < 0 {
			h.store.mu.Unlock()
			badRequest(w, "stock must be non-negative")
			return
		}
		p.Stock = *req.Stock
	}
	p.UpdatedAt = time.Now().UTC()
	h.store.mu.Unlock()

	writeJSON(w, http.StatusOK, p)
}

// deleteProduct handles DELETE /products/{id}.
func (h *Handler) deleteProduct(w http.ResponseWriter, _ *http.Request, id string) {
	h.store.mu.Lock()
	_, ok := h.store.products[id]
	if ok {
		delete(h.store.products, id)
	}
	h.store.mu.Unlock()

	if !ok {
		notFound(w, id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateCreate returns an error if required fields are missing or invalid.
func validateCreate(req CreateProductRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if req.Price < 0 {
		return fmt.Errorf("price must be non-negative")
	}
	if req.Stock < 0 {
		return fmt.Errorf("stock must be non-negative")
	}
	return nil
}

// parsePagination reads limit and offset from query params.
func parsePagination(r *http.Request) (limit, offset int) {
	limit = 100
	offset = 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

// writeJSON serializes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// badRequest writes a 400 JSON error.
func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

// notFound writes a 404 JSON error.
func notFound(w http.ResponseWriter, id string) {
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": fmt.Sprintf("product %q not found", id),
	})
}

// methodNotAllowed writes a 405 JSON error.
func methodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}
