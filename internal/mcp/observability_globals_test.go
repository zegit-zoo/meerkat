package mcp

import (
	"fmt"

	"go.opentelemetry.io/otel"
)

// globalTracerProviderPointer identifies the process-global
// OpenTelemetry tracer provider by identity, so a test can assert that
// building a server left it alone.
//
// It compares by formatted pointer rather than by value because
// otel.GetTracerProvider returns an interface whose dynamic type is not
// guaranteed comparable, and a panic inside an equality check would be a
// confusing way to learn that.
func globalTracerProviderPointer() string {
	return fmt.Sprintf("%p", otel.GetTracerProvider())
}
