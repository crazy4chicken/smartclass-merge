// Command genspec walks the HTTP router and writes docs/public/openapi.yaml,
// the document the VitePress API reference pages are rendered from.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	apidocs "github.com/crazy4chicken/nsc-teamusers/apidocs/go"

	"github.com/crazy4chicken/smartclass-merge/internal/httpapi"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// generate collects the documented operations and writes the OpenAPI document.
func generate() error {
	// chi.Walk only inspects route patterns, so the router is built from
	// zero-valued dependencies: no database or network connection is opened at
	// generation time.
	router := httpapi.New(httpapi.Deps{})

	operations, err := apidocs.Collect(router, httpapi.DocOperations, httpapi.DocPermissionDeriver)
	if err != nil {
		return fmt.Errorf("collect API operations: %w", err)
	}

	path := filepath.Join("docs", "public", "openapi.yaml")
	if err := writeSpec(path, operations); err != nil {
		return fmt.Errorf("write OpenAPI document: %w", err)
	}
	return nil
}

// writeSpec renders the document atomically, so a failed generation never
// leaves a truncated spec for the docs build to consume.
func writeSpec(path string, operations []apidocs.Operation) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".openapi.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)

	options := apidocs.EmitOptions{
		Title:   "SmartClass Merge API",
		Version: "0.1.0",
		Servers: []apidocs.Server{{
			URL:         "http://localhost:8083",
			Description: "Local server (default MERGE_LISTEN_ADDR)",
		}},
		SecurityScheme: apidocs.SecurityScheme{
			Name:         "bearerAuth",
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "EdDSA JWT",
		},
		PermissionExtension: "x-teamusers-permission",
	}
	if err := apidocs.Emit(operations, temporary, options); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("emit document: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace document: %w", err)
	}
	return nil
}
