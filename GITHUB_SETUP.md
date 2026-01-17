# TaskFlow - GitHub Setup Guide

This guide will help you set up your TaskFlow repository on GitHub with organized commits.

## Prerequisites

- Git installed on your machine
- GitHub account
- GitHub CLI (optional, but recommended)

## Step 1: Prepare Your Repository

### 1.1 Update Personal Information

Edit the following files and replace placeholders:

**README.md:**
- Replace `YOUR_USERNAME` with your GitHub username
- Replace `[Your Name]` with your name
- Update the email addresses

**LICENSE:**
- Replace `[Your Name]` with your name
- Update the year if needed

**go.mod:**
- Replace `github.com/dedezza1D/taskflow` with `github.com/YOUR_USERNAME/taskflow`

### 1.2 Configure Git
```bash
# Set your git identity
git config user.name "Your Name"
git config user.email "your.email@example.com"
```

## Step 2: Initialize Git Repository
```bash
# Initialize git (if not already done)
git init

# Rename branch to main
git branch -M main

# Add all files
git add .

# Create initial commit
git commit -m "Initial commit: TaskFlow distributed task queue"
```

## Step 3: Create GitHub Repository

### Option A: Using GitHub CLI (Recommended)
```bash
# Install GitHub CLI if needed
# Windows: winget install GitHub.cli
# macOS: brew install gh
# Linux: See https://github.com/cli/cli#installation

# Login to GitHub
gh auth login

# Create and push repository
gh repo create taskflow --public --source=. --remote=origin --push

# Or for private repository
gh repo create taskflow --private --source=. --remote=origin --push
```

### Option B: Using Web Interface

1. Go to https://github.com/new
2. Create a new repository named `taskflow`
3. **Do NOT** initialize with README, .gitignore, or license (we already have these)
4. Copy the repository URL
```bash
# Add remote
git remote add origin https://github.com/YOUR_USERNAME/taskflow.git

# Push all commits
git push -u origin main
```

## Step 4: Verify Repository

After pushing, verify your repository:

1. Check all files are visible
2. Verify README renders correctly
3. Check that License is recognized
4. Test clone on another machine

## Step 5: Add Repository Features

### 5.1 Add Topics

On GitHub, click "Add topics" and add:
- `golang`
- `distributed-systems`
- `task-queue`
- `nats-jetstream`
- `postgresql`
- `docker`
- `microservices`

### 5.2 Add Description

Set repository description:
```
A production-ready distributed task queue and document processing platform built with Go, NATS JetStream, and PostgreSQL
```

### 5.3 Enable Discussions (Optional)

1. Go to Settings → General → Features
2. Enable Discussions

## Step 6: Create Initial Release

### Using GitHub CLI
```bash
# Create a tag
git tag -a v0.1.0 -m "Initial release"
git push origin v0.1.0

# Create release
gh release create v0.1.0 \
  --title "TaskFlow v0.1.0" \
  --notes "Initial release of TaskFlow - distributed task queue system"
```

### Using Web Interface

1. Go to Releases → Draft a new release
2. Choose tag: `v0.1.0`
3. Title: "TaskFlow v0.1.0"
4. Description: Add release notes
5. Publish release

## Troubleshooting

### Issue: Large files
```bash
# Check file sizes
dir /s

# Use Git LFS if needed
git lfs install
git lfs track "*.large-extension"
```

## Next Steps

After setting up GitHub:

1. **Add CI/CD** - Set up GitHub Actions
2. **Enable Dependabot** - Automated dependency updates
3. **Add branch protection** - Protect main branch
4. **Create issues** - Start tracking work

Good luck with your TaskFlow project! 🚀