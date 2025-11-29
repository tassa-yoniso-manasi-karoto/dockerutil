# DOCKERUTIL DEVELOPMENT GUIDE

## Build & Test Commands
- Build: `go build ./...`
- Test: `go test ./...`
- Test single file: `go test ./path/to/file_test.go`
- Test with coverage: `go test -cover ./...`
- Lint: `golangci-lint run`
- Format code: `gofmt -w .`

## Code Style Guidelines
- **Imports**: Group imports: standard lib first, then external packages, then local packages
- **Error Handling**: Always wrap errors with fmt.Errorf("context: %w", err)
- **Logging**: Use zerolog for structured logging (rs/zerolog package)
- **Documentation**: Document all exported functions, types, and variables
- **Naming**: Use camelCase for private and PascalCase for public identifiers
- **Context**: Pass context.Context as first parameter in functions that interact with Docker
- **File Structure**: Keep related functionality in the same file
- **Error Types**: Define custom error variables using `var ErrXxx = errors.New("xxx")`
- **Indentation**: Always use tabs for indentation, not spaces

## Context and Docker Manager Guidelines

### Context Usage
- Always pass context as first parameter in public functions
- Never store context in struct fields
- Use context.WithTimeout for API calls that may time out
- Always cancel the context when done
- Avoid using package-level context variables

### Docker Manager Patterns
- Use functional options pattern for configuring managers
- Provide backward compatibility through package-level functions
- Support multiple instances through manager-based APIs
- Follow the pattern used in go-aksharamukha and go-ichiran
- Prefer dependency injection over global state

### Example Manager Creation
```go
// Create a custom manager with options
manager, err := NewManager(ctx, 
    WithProjectName("custom-name"),
    WithQueryTimeout(5*time.Minute))
if err != nil {
    return err
}

// Use the manager
err = manager.Init(ctx)

// Clean up when done
defer manager.Close()
```