# Agent Blueprints Repository Setup Instructions

## Repository: https://github.com/oat-agent/agent-blueprints

## Purpose
This repository contains specialized agent templates for the Open Agent Teams (OAT) system. These templates define domain-specific agents that can be dynamically instantiated by the OAT factory system.

## Required Structure

```
agent-blueprints/
├── README.md                          # Main documentation
├── registry.yaml                      # Template registry/manifest
├── templates/
│   ├── security/
│   │   ├── security-auditor.yaml
│   │   ├── vulnerability-scanner.yaml
│   │   └── compliance-checker.yaml
│   ├── performance/
│   │   ├── performance-profiler.yaml
│   │   ├── load-tester.yaml
│   │   └── benchmark-runner.yaml
│   ├── database/
│   │   ├── database-migrator.yaml
│   │   ├── schema-validator.yaml
│   │   └── data-synchronizer.yaml
│   ├── documentation/
│   │   ├── api-documenter.yaml
│   │   ├── readme-generator.yaml
│   │   └── changelog-maintainer.yaml
│   ├── testing/
│   │   ├── integration-tester.yaml
│   │   ├── e2e-tester.yaml
│   │   └── chaos-engineer.yaml
│   └── devops/
│       ├── ci-cd-optimizer.yaml
│       ├── docker-builder.yaml
│       ├── kubernetes-deployer.yaml
│       └── terraform-planner.yaml
├── examples/
│   ├── usage-basic.md
│   ├── usage-advanced.md
│   └── custom-template.yaml
├── docs/
│   ├── template-development.md
│   ├── capability-reference.md
│   └── best-practices.md
└── scripts/
    ├── validate.sh                    # Validate all templates
    ├── install.sh                     # Install templates to OAT
    └── test.sh                        # Test template functionality
```

## Template Schema

Each template MUST follow this schema:

```yaml
apiVersion: agents.oat.dev/v1
kind: AgentTemplate
metadata:
  name: template-name
  version: 1.0.0
  author: author-name
  description: Clear description of agent purpose
  tags: [tag1, tag2, tag3]

spec:
  base:
    type: worker|persistent|review
    model: claude-3-opus|gpt-4|default
    temperature: 0.0-1.0
    
  capabilities:
    tools:
      - name: tool-name
        version: ">=1.0.0"
    apis:
      - github
      - datadog
    models:
      primary: model-name
      secondary: fallback-model
  
  resources:
    memory: 2Gi
    cpu: 2
    timeout: 30m
    api_limits:
      tokens_per_minute: 100000
      requests_per_minute: 60
  
  prompt:
    system: |
      Agent system prompt here
    task_template: |
      Task: {task_description}
      Repository: {repo_name}
  
  behavior:
    auto_complete: true
    require_verification: false
    pr_creation: required|optional|none
    
  success:
    conditions:
      - type: file_exists|command_success|test_pass
        path: optional/path
        command: optional command
```

## Priority Templates to Create

### 1. Security Auditor (templates/security/security-auditor.yaml)
- Scans for vulnerabilities using semgrep, gitleaks, trivy
- Generates security reports
- Creates PRs with security fixes

### 2. Performance Profiler (templates/performance/performance-profiler.yaml)
- Runs performance benchmarks
- Identifies bottlenecks
- Suggests optimizations

### 3. Database Migrator (templates/database/database-migrator.yaml)
- Creates database migrations
- Validates schema changes
- Handles rollback scenarios

### 4. API Documenter (templates/documentation/api-documenter.yaml)
- Generates OpenAPI/Swagger docs
- Creates API usage examples
- Maintains API changelog

### 5. Integration Tester (templates/testing/integration-tester.yaml)
- Writes integration tests
- Sets up test environments
- Validates API contracts

## Integration with OAT Factory

The OAT factory (in main repo) will fetch templates from this repository:

```go
// In OAT main repo
registry := factory.NewTemplateRegistry()
registry.FetchFromRegistry("https://raw.githubusercontent.com/oat-agent/agent-blueprints/main")

// Create agent from template
factory.CreateFromTemplate(ctx, "security-auditor", map[string]interface{}{
    "task": "Audit authentication system",
    "repository": "my-repo",
    "focus_areas": "auth,jwt,sessions",
})
```

## Registry Manifest (registry.yaml)

```yaml
version: 1.0.0
templates:
  - name: security-auditor
    path: templates/security/security-auditor.yaml
    author: security-team
    verified: true
    stability: stable
    
  - name: performance-profiler
    path: templates/performance/performance-profiler.yaml
    author: perf-team
    verified: true
    stability: stable
    
  - name: database-migrator
    path: templates/database/database-migrator.yaml
    author: data-team
    verified: true
    stability: beta
```

## Key Implementation Notes

1. **DO NOT duplicate factory code** - This repo contains ONLY templates
2. **Templates are YAML definitions** - No Go code needed here
3. **Focus on agent behavior** - Define what agents do, not how factory works
4. **Each template is self-contained** - Include all needed configuration
5. **Use semantic versioning** - Templates will evolve over time
6. **Document requirements** - Clear tool/API dependencies
7. **Provide examples** - Show how to use each template

## Testing Templates

Before committing templates:

1. Validate YAML syntax
2. Check required fields
3. Test with OAT factory (from main repo)
4. Verify tool availability
5. Document any special setup

## Success Criteria

- [ ] Repository structure created
- [ ] At least 5 specialized templates implemented
- [ ] Registry manifest complete
- [ ] Documentation for template development
- [ ] Validation scripts working
- [ ] Examples provided
- [ ] Integration tested with OAT factory

## Next Steps

1. Create the repository structure
2. Implement priority templates
3. Set up validation scripts
4. Test integration with OAT factory
5. Document usage patterns
6. Enable community contributions