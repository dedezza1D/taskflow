# Contributing to TaskFlow

First off, thank you for considering contributing to TaskFlow! It's people like you that make TaskFlow a great tool.

## How Can I Contribute?

### Reporting Bugs

Before creating bug reports, please check the existing issues to avoid duplicates. When you create a bug report, include as many details as possible:

**Bug Report Template:**
**Describe the bug**
A clear and concise description of what the bug is.

**To Reproduce**
Steps to reproduce the behavior:
1. Go to '...'
2. Run '...'
3. See error

**Expected behavior**
A clear description of what you expected to happen.

**Environment:**
- OS: [e.g. Ubuntu 22.04]
- Go version: [e.g. 1.21.0]
- TaskFlow version: [e.g. v0.1.0]
- Docker version: [if applicable]

**Logs**
```
Paste relevant logs here
```

**Additional context**
Add any other context about the problem here.
```

### Suggesting Enhancements

Enhancement suggestions are tracked as GitHub issues. When creating an enhancement suggestion, include:

- **Clear title** describing the enhancement
- **Detailed description** of the proposed functionality
- **Use cases** showing why this would be useful
- **Possible implementation** if you have ideas

### Pull Requests

1. **Fork the repository** and create your branch from `main`
2. **Make your changes** following our coding standards
3. **Add tests** if you've added code that should be tested
4. **Update documentation** if you've changed APIs or added features
5. **Ensure tests pass** by running tests
6. **Format your code** with `go fmt`
7. **Create a pull request** with a clear description

## Development Setup

### Prerequisites

- Go 1.21 or higher
- Docker and Docker Compose
- Make (optional but recommended)

### Setting Up Your Development Environment
```bash
# Clone your fork
git clone https://github.com/YOUR_USERNAME/taskflow.git
cd taskflow

# Add upstream remote
git remote add upstream https://github.com/ORIGINAL_OWNER/taskflow.git

# Install dependencies
go mod download

# Start infrastructure
docker-compose up -d postgres nats

# Run migrations
psql $DATABASE_URL < api/deployments/migrations/001_init.sql

# Run tests
go test ./...
```

### Running the Project Locally
```bash
# Terminal 1: Start API
go run cmd/api/main.go

# Terminal 2: Start Worker
go run cmd/worker/main.go

# Terminal 3: Test
curl http://localhost:8080/health
```

## Coding Standards

### Go Style Guide

We follow the official [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments) and [Effective Go](https://golang.org/doc/effective_go.html).

**Key points:**

- Use `gofmt` for formatting
- Follow standard Go naming conventions
- Write clear, self-documenting code
- Add comments for exported functions and types
- Keep functions small and focused
- Handle errors explicitly

**Example:**
```go
// Good
func ProcessTask(ctx context.Context, task *Task) error {
    if task == nil {
        return errors.New("task cannot be nil")
    }
    
    // Process the task
    result, err := doWork(ctx, task)
    if err != nil {
        return fmt.Errorf("failed to process task: %w", err)
    }
    
    return nil
}

// Bad
func ProcessTask(ctx context.Context, task *Task) error {
    result, err := doWork(ctx, task)
    return err  // Missing error wrapping and validation
}
```

### Commit Messages

We use [Conventional Commits](https://www.conventionalcommits.org/) format:
```
<type>(<scope>): <subject>

<body>

<footer>
```

**Types:**
- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation only
- `style`: Code style (formatting, missing semicolons, etc.)
- `refactor`: Code refactoring
- `perf`: Performance improvements
- `test`: Adding or updating tests
- `chore`: Build process or tooling changes

**Examples:**
```bash
feat(api): add pagination to task list endpoint

Add limit and offset query parameters to support pagination.
Includes tests and documentation updates.

Closes #123
```
```bash
fix(worker): prevent duplicate task execution

Add optimistic locking to prevent race condition where
multiple workers could claim the same task.

Fixes #456
```

### Testing

- Write unit tests for all new code
- Aim for >80% code coverage
- Use table-driven tests where appropriate
- Mock external dependencies

**Example test:**
```go
func TestProcessTask(t *testing.T) {
    tests := []struct {
        name    string
        task    *Task
        want    error
        wantErr bool
    }{
        {
            name: "valid task",
            task: &Task{ID: uuid.New(), Type: "test"},
            want: nil,
            wantErr: false,
        },
        {
            name: "nil task",
            task: nil,
            want: nil,
            wantErr: true,
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            err := ProcessTask(context.Background(), tt.task)
            if (err != nil) != tt.wantErr {
                t.Errorf("ProcessTask() error = %v, wantErr %v", err, tt.wantErr)
            }
        })
    }
}
```

### Documentation

- Update README.md for user-facing changes
- Add godoc comments for exported types and functions
- Update API documentation for endpoint changes
- Include examples in documentation

## Pull Request Process

1. **Update your branch** with the latest from `main`:
```bash
   git checkout main
   git pull upstream main
   git checkout your-branch
   git rebase main
```

2. **Run all checks**:
```bash
   go test ./...
   go fmt ./...
```

3. **Create pull request** with description including:
   - What changes were made and why
   - How to test the changes
   - Screenshots (if UI changes)
   - Related issue numbers

4. **Wait for review** - maintainers will review your PR

5. **Address feedback** - make requested changes

6. **Merge** - once approved, a maintainer will merge your PR

## Questions?

Feel free to:
- Open an issue for questions
- Start a discussion in GitHub Discussions
- Reach out to maintainers

Thank you for contributing to TaskFlow! 🎉