# Complete Codebase Intelligence and DAG Graph System Specification

## Executive Summary
This specification defines a comprehensive codebase intelligence system for Open Agent Teams (OATs), reimplementing all core DAG functionality in Go. The system provides deep codebase understanding through AST parsing, dependency graph analysis, and AI-powered documentation generation, specifically designed to enhance the planner agent's ability to understand, navigate, and modify codebases intelligently.

## System Overview

### Vision
Provide the OATs planner agent with "X-ray vision" into any codebase, enabling it to:
- Understand the complete architecture and dependencies
- Generate precise, file-level task decompositions
- Predict impact radius of changes
- Identify optimal execution ordering
- Detect dead code and security issues
- Generate comprehensive documentation

### Core Capabilities 
1. **Multi-language AST parsing** (16+ languages)
2. **Dependency graph construction** with PageRank and community detection
3. **AI-powered documentation generation** with wiki pages
4. **Dead code detection** with confidence scoring
5. **Security scanning** for vulnerable patterns
6. **Git intelligence** with hotspot and ownership analysis
7. **Vector search** for semantic code understanding
8. **Real-time indexing** with incremental updates
9. **Interactive visualization** with web dashboard
10. **Multi-repository workspace** support

### Integration with OATs Architecture
- **Daemon Integration**: Runs as part of the OATs daemon process
- **Planner Enhancement**: Provides context for task decomposition
- **Agent Coordination**: Shares index across all agent worktrees
- **TUI Integration**: Displays codebase insights in the terminal UI
- **Model Routing**: Uses OATs' existing LLM provider infrastructure

## Architecture Within OATs

### Directory Structure
```
open-agent-teams-8/
├── internal/
│   ├── codeindex/              # Core indexing system
│   │   ├── parser/              # AST parsing per language
│   │   │   ├── parser.go        # Base parser interface
│   │   │   ├── golang.go        # Go-specific parser
│   │   │   ├── typescript.go    # TypeScript/JavaScript parser
│   │   │   ├── python.go        # Python parser
│   │   │   └── ...              # Other language parsers
│   │   ├── graph/               # Graph algorithms
│   │   │   ├── builder.go       # Graph construction
│   │   │   ├── pagerank.go      # PageRank implementation
│   │   │   ├── community.go     # Louvain/Leiden algorithms
│   │   │   └── analysis.go      # Graph analysis functions
│   │   ├── index/               # Indexing engine
│   │   │   ├── indexer.go       # Main indexer
│   │   │   ├── incremental.go   # Incremental updates
│   │   │   └── cache.go         # Index caching
│   │   ├── query/               # Query engine
│   │   │   ├── search.go        # Search functionality
│   │   │   ├── context.go       # Context assembly
│   │   │   └── vector.go        # Vector search
│   │   ├── generate/            # Documentation generation
│   │   │   ├── wiki.go          # Wiki page generation
│   │   │   ├── templates.go     # Page templates
│   │   │   └── llm.go           # LLM integration
│   │   ├── deadcode/            # Dead code detection
│   │   │   ├── detector.go      # Detection algorithms
│   │   │   └── confidence.go    # Confidence scoring
│   │   └── security/            # Security scanning
│   │       ├── scanner.go       # Pattern scanning
│   │       └── patterns.go      # Security patterns
│   ├── planner/                 # Enhanced planner
│   │   ├── planner.go           # Main planner logic
│   │   ├── context.go           # Codebase context integration
│   │   ├── decompose.go         # Task decomposition with code intelligence
│   │   └── execute.go           # Execution planning with dependencies
│   └── daemon/                  # Daemon integration
│       └── codeindex_manager.go # Manages indexing lifecycle
├── pkg/
│   └── codeindex/               # Public API
│       ├── api.go               # External API
│       └── types.go             # Shared types
└── web/
    └── codeindex/               # Web UI assets
        ├── dashboard.html        # Interactive dashboard
        └── graph.js              # Graph visualization
```

## Detailed Component Specifications

### 1. Multi-Language AST Parser System

```go
package codeindex

import (
    "github.com/smacker/go-tree-sitter"
    "github.com/smacker/go-tree-sitter/golang"
    "github.com/smacker/go-tree-sitter/javascript"
    "github.com/smacker/go-tree-sitter/python"
)

type Parser struct {
    parsers map[string]*sitter.Parser
}

type Symbol struct {
    Name      string
    Type      string // function, class, variable, import
    Line      int
    Column    int
    EndLine   int
    EndColumn int
}

type ParsedFile struct {
    Path         string
    Language     string
    Symbols      []Symbol
    Imports      []string
    Exports      []string
    Dependencies []string
}

// Language detection and routing
func (p *Parser) ParseFile(path string) (*ParsedFile, error) {
    lang := p.detectLanguage(path)
    parser := p.getParserForLanguage(lang)
    return parser.Parse(path)
}

// Language-specific parser interface
type LanguageParser interface {
    Parse(path string) (*ParsedFile, error)
    ExtractSymbols(ast *sitter.Tree) []Symbol
    ExtractImports(ast *sitter.Tree) []Import
    ExtractExports(ast *sitter.Tree) []Export
    ResolveImport(imp Import, searchPaths []string) *ResolvedImport
}

// Enhanced symbol extraction with full metadata
type Symbol struct {
    Name           string
    Type           SymbolType // function, class, interface, variable, etc.
    Visibility     Visibility // public, private, protected
    Location       Location
    Signature      string     // Full signature for functions/methods
    DocComment     string     // Extracted documentation
    Decorators     []string   // Annotations/decorators
    Heritage       []string   // What this extends/implements
    References     []Location // Where this symbol is used
    IsTest         bool       // Test function/class
    IsDeprecated   bool       // Marked as deprecated
    Complexity     int        // Cyclomatic complexity for functions
}

// Import tracking with resolution
type Import struct {
    Source         string     // Raw import string
    Symbols        []string   // Specific imported symbols
    IsRelative     bool       // Relative vs absolute import
    IsFramework    bool       // Framework-specific import
    ResolvedPath   string     // Resolved file path
    ImportType     ImportType // static, dynamic, lazy
}
```

### 2. Advanced Graph Construction and Analysis

```go
package codeindex

import (
    "github.com/dominikbraun/graph"
)

type CodeGraph struct {
    graph       graph.Graph[string, *FileNode]
    communities map[string][]string
    pagerank    map[string]float64
}

type FileNode struct {
    Path         string
    Symbols      []Symbol
    ImportCount  int
    ExportCount  int
    Centrality   float64
    Community    string
}

// Complete graph building with all algorithms
func (g *CodeGraph) Build(files []*ParsedFile) error {
    // Phase 1: Build file-level graph
    g.buildFileGraph(files)
    
    // Phase 2: Build symbol-level graph
    g.buildSymbolGraph(files)
    
    // Phase 3: Add framework-specific edges
    g.addFrameworkEdges(files)
    
    // Phase 4: Calculate graph metrics
    g.calculatePageRank()
    g.calculateBetweennessCentrality()
    g.detectStronglyConnectedComponents()
    
    // Phase 5: Community detection
    g.detectCommunities() // Leiden algorithm (preferred over Louvain)
    g.splitOversizedCommunities()
    g.labelCommunities()
    
    // Phase 6: Identify architectural patterns
    g.identifyLayers()        // Presentation, business, data layers
    g.detectCircularDeps()     // Find and report cycles
    g.findBridgeComponents()   // Critical architectural bridges
    
    return nil
}

// PageRank implementation
func (g *CodeGraph) calculatePageRank() {
    const (
        dampingFactor = 0.85
        maxIterations = 100
        tolerance     = 1e-6
    )
    
    // Initialize PageRank values
    n := float64(g.NodeCount())
    for _, node := range g.nodes {
        node.PageRank = 1.0 / n
    }
    
    // Iterative calculation
    for i := 0; i < maxIterations; i++ {
        newRanks := make(map[string]float64)
        maxDiff := 0.0
        
        for id, node := range g.nodes {
            rank := (1 - dampingFactor) / n
            for _, incoming := range g.GetIncomingEdges(id) {
                sourceRank := g.nodes[incoming.Source].PageRank
                outDegree := float64(g.GetOutDegree(incoming.Source))
                rank += dampingFactor * (sourceRank / outDegree)
            }
            newRanks[id] = rank
            
            diff := math.Abs(rank - node.PageRank)
            if diff > maxDiff {
                maxDiff = diff
            }
        }
        
        // Update ranks
        for id, rank := range newRanks {
            g.nodes[id].PageRank = rank
        }
        
        // Check convergence
        if maxDiff < tolerance {
            break
        }
    }
}

// Leiden community detection (improved version of Louvain)
func (g *CodeGraph) detectCommunities() {
    // Phase 1: Local moving
    // Phase 2: Refinement (Leiden improvement over Louvain)
    // Phase 3: Network aggregation
    // Returns communities with quality score (modularity)
}

func (g *CodeGraph) GetDependencies(filepath string) []string {
    // Return direct dependencies of a file
}

func (g *CodeGraph) GetImpactRadius(filepath string) []string {
    // Return files that would be affected by changes
}
```

### 3. Comprehensive Indexing Engine

```go
package codeindex

import (
    "path/filepath"
    "github.com/go-git/go-git/v5"
)

type Indexer struct {
    parser *Parser
    graph  *CodeGraph
    cache  *IndexCache
}

type IndexResult struct {
    TotalFiles   int
    TotalSymbols int
    Communities  []Community
    KeyFiles     []string // Files with highest PageRank
}

type Community struct {
    ID    string
    Files []string
    Name  string // Auto-generated from common path/imports
}

// Full repository indexing with all DAG features
func (i *Indexer) IndexRepository(path string) (*IndexResult, error) {
    ctx := context.Background()
    
    // Phase 1: File traversal with .gitignore respect
    files, err := i.traverseRepository(path)
    if err != nil {
        return nil, err
    }
    
    // Phase 2: Parallel AST parsing (all CPU cores)
    parsed := i.parseFilesParallel(files)
    
    // Phase 3: Git intelligence extraction
    gitData := i.extractGitIntelligence(path)
    
    // Phase 4: Graph construction
    graph := i.buildGraph(parsed)
    
    // Phase 5: Dead code detection
    deadCode := i.detectDeadCode(graph, gitData)
    
    // Phase 6: Security scanning
    securityFindings := i.scanSecurity(parsed)
    
    // Phase 7: Generate embeddings for vector search
    embeddings := i.generateEmbeddings(parsed)
    
    // Phase 8: Documentation generation (if enabled)
    if i.config.GenerateDocs {
        i.generateDocumentation(parsed, graph)
    }
    
    // Phase 9: Persist to database
    i.persist(ctx, graph, deadCode, securityFindings, embeddings)
    
    // Phase 10: Update search indices
    i.updateSearchIndices()
    
    return &IndexResult{
        TotalFiles:       len(files),
        ParsedFiles:      len(parsed),
        TotalSymbols:     i.countSymbols(parsed),
        Communities:      graph.GetCommunities(),
        KeyFiles:         graph.GetTopPageRankFiles(10),
        DeadCode:         deadCode,
        SecurityIssues:   securityFindings,
        IndexTime:        time.Since(startTime),
        HotSpots:         gitData.GetHotSpots(),
        Ownership:        gitData.GetOwnership(),
    }, nil
}

// Incremental indexing for file changes
func (i *Indexer) UpdateIndex(changedFiles []string) error {
    // Determine impact radius
    affected := i.graph.GetAffectedFiles(changedFiles)
    
    // Re-parse affected files
    parsed := i.parseFiles(affected)
    
    // Update graph incrementally
    i.graph.UpdateNodes(parsed)
    
    // Recalculate metrics for affected communities
    communities := i.graph.GetAffectedCommunities(affected)
    for _, comm := range communities {
        i.graph.RecalculateCommunityMetrics(comm)
    }
    
    // Update embeddings
    i.updateEmbeddings(parsed)
    
    return nil
}

func (i *Indexer) UpdateIndex(changedFiles []string) error {
    // Incremental update for changed files
    // Recalculate affected portions of graph
}
```

### 4. Advanced Query and Context Engine

```go
package codeindex

type QueryEngine struct {
    graph   *CodeGraph
    indexer *Indexer
}

type QueryResult struct {
    Files       []FileMatch
    Symbols     []SymbolMatch
    Communities []string
}

type FileMatch struct {
    Path       string
    Relevance  float64
    Reason     string // "defines symbol X", "imports Y", etc.
}

// Hybrid search combining vector, full-text, and graph traversal
func (q *QueryEngine) Search(query string) (*QueryResult, error) {
    // Phase 1: Vector search for semantic similarity
    vectorResults := q.vectorSearch(query, 20)
    
    // Phase 2: Full-text search with ranking
    textResults := q.fullTextSearch(query, 20)
    
    // Phase 3: Symbol search with fuzzy matching
    symbolResults := q.symbolSearch(query, 20)
    
    // Phase 4: Graph-based relevance boosting
    // Boost results based on PageRank and centrality
    results := q.mergeAndRank(vectorResults, textResults, symbolResults)
    
    // Phase 5: Add context for each result
    for i, result := range results {
        results[i].Context = q.getResultContext(result)
        results[i].Dependencies = q.graph.GetDependencies(result.Path)
        results[i].Community = q.graph.GetCommunity(result.Path)
    }
    
    return results, nil
}

// Context assembly for LLM prompts (critical for planner)
func (q *QueryEngine) AssembleContext(files []string, maxTokens int) *Context {
    budget := &TokenBudget{
        Max:       maxTokens,
        Remaining: maxTokens,
    }
    
    ctx := &Context{
        Files:        []FileContext{},
        Symbols:      []SymbolContext{},
        Dependencies: map[string][]string{},
        Communities:  map[string]Community{},
        GitHistory:   []GitEvent{},
    }
    
    // Priority 1: Source code of requested files
    for _, file := range files {
        if budget.CanFit(file) {
            ctx.Files = append(ctx.Files, q.getFileContext(file))
            budget.Consume(file)
        }
    }
    
    // Priority 2: Direct dependencies
    deps := q.graph.GetDirectDependencies(files)
    for _, dep := range deps {
        if budget.CanFit(dep) {
            ctx.Dependencies[dep] = q.getFileSignatures(dep)
            budget.Consume(dep)
        }
    }
    
    // Priority 3: Community context
    communities := q.graph.GetCommunitiesForFiles(files)
    for _, comm := range communities {
        if budget.CanFit(comm) {
            ctx.Communities[comm.ID] = comm
            budget.Consume(comm)
        }
    }
    
    // Priority 4: Git history and ownership
    if budget.Remaining > 1000 {
        ctx.GitHistory = q.git.GetRecentHistory(files, 10)
        ctx.Ownership = q.git.GetOwnership(files)
    }
    
    return ctx
}

func (q *QueryEngine) GetContext(filepath string, maxTokens int) string {
    // Get relevant context for a file
    // Include dependencies, dependents, and community
}
```

## Deep Planner Agent Integration

### Lifecycle and Triggering

```go
// When planner agent starts
func (p *PlannerAgent) Initialize() {
    // Auto-index on startup if not cached
    if !p.codeindex.HasValidCache() {
        p.ui.ShowStatus("Indexing codebase for intelligence...")
        result, err := p.codeindex.IndexRepository(".")
        if err != nil {
            p.ui.ShowWarning("Proceeding without code intelligence")
        } else {
            p.ui.ShowSuccess(fmt.Sprintf("Indexed %d files, found %d communities", 
                result.TotalFiles, len(result.Communities)))
        }
    }
}

// Trigger points for re-indexing
func (p *PlannerAgent) HandleFileChange(event FileChangeEvent) {
    // Incremental update on file changes
    p.codeindex.UpdateIndex(event.ChangedFiles)
}

func (p *PlannerAgent) HandleGitOperation(event GitEvent) {
    switch event.Type {
    case GitPull, GitMerge, GitCheckout:
        // Re-index after major git operations
        p.codeindex.RefreshIndex()
    }
}
```

### Enhanced Planning Workflow

```go
package planner

import "github.com/Root-IO-Labs/open-agent-teams/internal/codeindex"

type PlannerContext struct {
    index     *codeindex.Indexer
    query     *codeindex.QueryEngine
    workspace string
}

// Step 1: Requirement Analysis with Code Intelligence
func (p *Planner) AnalyzeRequirement(req string) *RequirementAnalysis {
    // Search codebase for relevant context
    searchResults := p.codeindex.Search(req)
    
    // Identify affected communities
    communities := p.identifyAffectedCommunities(searchResults)
    
    // Get architectural context
    architecture := p.codeindex.GetArchitecturalContext(communities)
    
    // Assess impact and risk
    impact := p.assessImpact(searchResults, communities)
    
    return &RequirementAnalysis{
        Requirement:        req,
        AffectedFiles:      searchResults.Files,
        AffectedCommunities: communities,
        Architecture:       architecture,
        ImpactAssessment:   impact,
        SuggestedApproach:  p.suggestApproach(impact),
    }
}

// Step 2: Task Decomposition with File-Level Precision
func (p *Planner) DecomposeIntoTasks(analysis *RequirementAnalysis) []Task {
    tasks := []Task{}
    
    // Create tasks based on affected communities
    for _, community := range analysis.AffectedCommunities {
        // Group related changes
        communityTasks := p.createCommunityTasks(community, analysis)
        tasks = append(tasks, communityTasks...)
    }
    
    // Add cross-cutting concerns
    if analysis.RequiresSchemaChange() {
        tasks = append(tasks, p.createSchemaTask(analysis))
    }
    
    if analysis.RequiresAPIChange() {
        tasks = append(tasks, p.createAPITask(analysis))
    }
    
    // Add test tasks based on impact
    testTasks := p.createTestTasks(analysis.ImpactAssessment)
    tasks = append(tasks, testTasks...)
    
    // Order tasks by dependency graph
    return p.orderTasksByDependencies(tasks)
}

// Step 3: Execution Planning with Parallelization
func (p *Planner) PlanExecution(tasks []Task) *ExecutionPlan {
    // Build task dependency graph
    taskGraph := p.buildTaskGraph(tasks)
    
    // Identify parallelizable waves
    waves := p.identifyExecutionWaves(taskGraph)
    
    // Assign agents based on expertise
    assignments := p.assignAgentsToTasks(waves)
    
    return &ExecutionPlan{
        Waves:       waves,
        Assignments: assignments,
        Timeline:    p.estimateTimeline(waves),
        Risks:       p.identifyRisks(tasks),
    }
}

func (pc *PlannerContext) GetImpactAnalysis(files []string) map[string][]string {
    // For each file, get its impact radius
    impact := make(map[string][]string)
    for _, file := range files {
        impact[file] = pc.index.graph.GetImpactRadius(file)
    }
    return impact
}

func (pc *PlannerContext) GetCodebaseStructure() string {
    // Return high-level structure with communities
    // Used in initial requirement analysis
}
```

### Task Generation with Code Intelligence

```go
type Task struct {
    ID                 string
    Title              string
    Description        string
    Type               TaskType // implementation, test, refactor, documentation
    
    // Code intelligence fields
    TargetFiles        []string            // Exact files to modify
    TargetSymbols      []string            // Specific functions/classes
    Dependencies       []string            // Other task IDs
    CodeDependencies   []string            // File dependencies from graph
    Community          string              // Which community this affects
    EstimatedImpact    ImpactLevel         // Based on PageRank/centrality
    
    // Execution metadata
    AcceptanceCriteria []string
    TestFiles          []string            // Tests to run after
    Wave               int                 // Execution wave (for parallelization)
    AssignedAgent      string              // Which agent will handle this
    
    // Risk assessment
    Risk               RiskLevel           // High/Medium/Low
    RiskFactors        []string            // Why this is risky
    Mitigations        []string            // How to reduce risk
}

// Generate task with full context
func (p *Planner) generateTask(requirement string, targetFile string) *Task {
    // Get file context
    fileInfo := p.codeindex.GetFileInfo(targetFile)
    
    // Get dependencies
    deps := p.codeindex.GetDependencies(targetFile)
    
    // Assess impact
    impact := p.assessFileImpact(fileInfo)
    
    // Identify tests
    tests := p.findRelatedTests(targetFile)
    
    return &Task{
        Title:            fmt.Sprintf("Modify %s for %s", filepath.Base(targetFile), requirement),
        TargetFiles:      []string{targetFile},
        TargetSymbols:    p.identifyTargetSymbols(fileInfo, requirement),
        CodeDependencies: deps,
        Community:        fileInfo.Community,
        EstimatedImpact:  impact,
        TestFiles:        tests,
        Risk:             p.assessRisk(impact, fileInfo.PageRank),
    }
}
```

### Intelligent Execution Orchestration

```go
// Smart execution ordering with dependency awareness
func (p *Planner) CreateExecutionWaves(tasks []Task) []ExecutionWave {
    waves := []ExecutionWave{}
    
    // Build dependency graph of tasks
    taskGraph := p.buildTaskDependencyGraph(tasks)
    
    // Topological sort with parallelization
    levels := taskGraph.TopologicalLevels()
    
    for i, level := range levels {
        wave := ExecutionWave{
            Number: i,
            Tasks:  level,
        }
        
        // Group by community for better agent utilization
        wave.Groups = p.groupTasksByCommunity(level)
        
        // Identify critical path
        wave.CriticalPath = p.findCriticalPath(level, taskGraph)
        
        // Risk assessment for wave
        wave.RiskLevel = p.assessWaveRisk(level)
        
        // Add gate conditions
        wave.Gates = p.defineGateConditions(level)
        
        waves = append(waves, wave)
    }
    
    return waves
}

// Monitor execution with impact tracking
func (p *Planner) MonitorExecution(wave ExecutionWave) *ExecutionStatus {
    status := &ExecutionStatus{
        Wave:      wave.Number,
        StartTime: time.Now(),
        Tasks:     make(map[string]TaskStatus),
    }
    
    // Track each task
    for _, task := range wave.Tasks {
        // Monitor file changes
        changes := p.codeindex.MonitorFileChanges(task.TargetFiles)
        
        // Validate changes against graph
        violations := p.validateChanges(changes, task)
        
        // Update impact assessment
        impact := p.codeindex.RecalculateImpact(changes)
        
        status.Tasks[task.ID] = TaskStatus{
            Changes:    changes,
            Violations: violations,
            Impact:     impact,
        }
    }
    
    return status
}
```

## User Experience (UX) Design

### Terminal UI Integration

```go
// In planner view
type PlannerView struct {
    // Existing fields...
    codeIntelligence *CodeIntelligencePanel
}

type CodeIntelligencePanel struct {
    ShowGraph        bool
    ShowCommunities  bool
    ShowHotspots     bool
    ShowDeadCode     bool
    CurrentFile      string
    FileInfo         *FileIntelligence
}

// Display in TUI
┌─ Planner Agent ─────────────────────────────────────────┐
│ Requirement: Add authentication to API                  │
│                                                         │
│ 📊 Code Intelligence:                                   │
│ ├─ Affected Communities: [auth, api, middleware]       │
│ ├─ Impact: 23 files, 145 symbols                       │
│ ├─ Risk: Medium (affects core API paths)               │
│ └─ Related Tests: 12 test files                        │
│                                                         │
│ 📋 Generated Tasks: (Wave 1 of 3)                      │
│ ├─ [1] Create auth middleware (internal/auth/)         │
│ ├─ [2] Add JWT validation (pkg/jwt/)                   │
│ └─ [3] Update API routes (internal/api/)               │
│                                                         │
│ 🔍 Current Focus: internal/api/router.go               │
│ ├─ PageRank: 0.0234 (top 5%)                          │
│ ├─ Community: api-core                                 │
│ ├─ Dependencies: 14 files                              │
│ └─ Last Modified: 2 days ago by alice                  │
└─────────────────────────────────────────────────────────┘
```

### Interactive Commands

```bash
# Planner commands with code intelligence
oat plan --with-intelligence "Add authentication"
oat plan show-impact              # Show impact analysis
oat plan show-graph               # ASCII graph visualization
oat plan show-communities          # List detected communities
oat plan show-dependencies <file> # Show file dependencies

# Direct codeindex commands
oat index                         # Index current repository
oat index --watch                 # Watch mode with auto-reindex
oat index stats                   # Show indexing statistics
oat index search "authentication" # Search codebase
oat index dead-code               # Show dead code report
oat index security                # Show security findings
oat index hotspots                # Show code hotspots

# Web dashboard
oat index serve                    # Start web UI on :3000
```

### Web Dashboard Integration

```go
// Serve dashboard at http://localhost:3000/codeindex
func (d *Daemon) ServeCodeIndexDashboard() {
    http.HandleFunc("/codeindex", d.handleDashboard)
    http.HandleFunc("/api/codeindex/graph", d.handleGraphAPI)
    http.HandleFunc("/api/codeindex/search", d.handleSearchAPI)
    http.HandleFunc("/api/codeindex/communities", d.handleCommunitiesAPI)
}
```

## Backend Architecture

### Storage Layer

```go
// Database schema (SQLite by default, PostgreSQL for scale)
type CodeIndexDB struct {
    // Core tables
    Files          []FileRecord
    Symbols        []SymbolRecord
    Edges          []EdgeRecord
    Communities    []CommunityRecord
    
    // Analysis tables
    DeadCode       []DeadCodeRecord
    SecurityIssues []SecurityRecord
    GitHistory     []GitRecord
    
    // Search tables
    Embeddings     []EmbeddingRecord
    SearchIndex    []SearchRecord
    
    // Cache tables
    PageRanks      []PageRankRecord
    Centralities   []CentralityRecord
}

// Storage location
// ~/.oat/codeindex/
//   ├── index.db          # SQLite database
//   ├── embeddings.bin    # Vector embeddings
//   ├── graph.cache       # Cached graph data
//   └── wiki/             # Generated documentation
```

### Daemon Integration

```go
// Daemon lifecycle management
func (d *Daemon) Start() error {
    // Start existing daemon components...
    
    // Initialize code indexer
    d.codeIndexer = codeindex.NewIndexer(
        codeindex.WithPath(d.workDir),
        codeindex.WithDatabase(d.dbPath),
        codeindex.WithLLMProvider(d.llmProvider),
    )
    
    // Initial indexing
    if err := d.codeIndexer.Initialize(); err != nil {
        log.Warn("Code intelligence unavailable: %v", err)
    }
    
    // Start file watcher
    d.startCodeIndexWatcher()
    
    return nil
}

// File watching for incremental updates
func (d *Daemon) startCodeIndexWatcher() {
    watcher := d.codeIndexer.NewWatcher()
    
    go func() {
        for event := range watcher.Events {
            if event.Op&fsnotify.Write == fsnotify.Write {
                d.codeIndexer.UpdateFile(event.Name)
            }
        }
    }()
}
```

## Complete Feature Implementation Timeline

### Phase 1: Core Foundation (Days 1-3)
- [ ] Set up package structure in `internal/codeindex/`
- [ ] Implement base parser interface
- [ ] Add Go language parser with tree-sitter
- [ ] Build basic file graph
- [ ] Create simple PageRank implementation
- [ ] Add SQLite storage

### Phase 2: Graph Intelligence (Days 4-6)
- [ ] Implement Leiden community detection
- [ ] Add betweenness centrality
- [ ] Build SCC detection
- [ ] Create impact analysis
- [ ] Add graph caching

### Phase 3: Planner Integration (Days 7-9)
- [ ] Integrate with planner context
- [ ] Enhance requirement analysis
- [ ] Add code-aware task decomposition
- [ ] Implement execution wave planning
- [ ] Add TUI panels

### Phase 4: Advanced Features (Days 10-12)
- [ ] Add TypeScript/JavaScript parser
- [ ] Add Python parser
- [ ] Implement dead code detection
- [ ] Add security scanning
- [ ] Create git intelligence

### Phase 5: Search and Query (Days 13-15)
- [ ] Implement vector embeddings
- [ ] Add semantic search
- [ ] Build full-text search
- [ ] Create context assembly
- [ ] Add search API

### Phase 6: Documentation Generation (Days 16-18)
- [ ] Create wiki templates
- [ ] Add LLM integration
- [ ] Build generation pipeline
- [ ] Implement caching
- [ ] Add markdown output

### Phase 7: Web Dashboard (Days 19-21)
- [ ] Create React dashboard
- [ ] Add graph visualization
- [ ] Build search interface
- [ ] Add community explorer
- [ ] Implement real-time updates

## Performance Requirements

### Benchmarks (Matching DAG)
- **Indexing Speed**: 
  - 1,000 files: < 5 seconds
  - 10,000 files: < 30 seconds
  - 100,000 files: < 5 minutes

- **Query Performance**:
  - Symbol search: < 50ms
  - Semantic search: < 200ms
  - Graph traversal: < 100ms
  - Context assembly: < 500ms

- **Memory Usage**:
  - Base overhead: < 50MB
  - Per 1000 files: < 10MB
  - Graph cache: < 100MB
  - Embeddings: < 500MB

- **Incremental Updates**:
  - Single file: < 100ms
  - Affected community: < 1 second
  - Full reindex: Avoid if possible

## Required Dependencies

### Go Modules
```go
require (
    // AST Parsing
    github.com/smacker/go-tree-sitter v0.0.0-20240625050157-a31a98a7c0f6
    github.com/smacker/go-tree-sitter/golang v0.0.0-20240625050157-a31a98a7c0f6
    github.com/smacker/go-tree-sitter/javascript v0.0.0-20240625050157-a31a98a7c0f6
    github.com/smacker/go-tree-sitter/python v0.0.0-20240625050157-a31a98a7c0f6
    github.com/smacker/go-tree-sitter/typescript/tsx v0.0.0-20240625050157-a31a98a7c0f6
    
    // Graph Algorithms
    github.com/dominikbraun/graph v0.23.0
    gonum.org/v1/gonum v0.15.0
    
    // Git Operations
    github.com/go-git/go-git/v5 v5.12.0
    
    // Database
    github.com/mattn/go-sqlite3 v1.14.22
    github.com/jmoiron/sqlx v1.3.5
    
    // Vector Operations
    github.com/philippgille/chromem-go v0.6.0
    
    // Web Dashboard
    github.com/gorilla/mux v1.8.1
    github.com/gorilla/websocket v1.5.1
    
    // Utilities
    github.com/fsnotify/fsnotify v1.7.0
    github.com/patrickmn/go-cache v2.1.0+incompatible
)
```

### Tree-sitter Grammar Files
```bash
# Download required .scm files
wget https://raw.githubusercontent.com/DAG-dev/DAG/main/packages/core/src/DAG/core/ingestion/queries/go.scm
wget https://raw.githubusercontent.com/DAG-dev/DAG/main/packages/core/src/DAG/core/ingestion/queries/typescript.scm
wget https://raw.githubusercontent.com/DAG-dev/DAG/main/packages/core/src/DAG/core/ingestion/queries/python.scm
```

## Complete Usage Examples

### Basic Usage

```go
// Initialize with full configuration
indexer := codeindex.NewIndexer(
    codeindex.WithPath("."),
    codeindex.WithDatabase("~/.oat/codeindex/index.db"),
    codeindex.WithLanguages([]string{"go", "typescript", "python"}),
    codeindex.WithLLMProvider(llmProvider),
    codeindex.WithParallel(runtime.NumCPU()),
)

// Initial indexing with progress
result, err := indexer.IndexRepository(".")
if err != nil {
    log.Fatal(err)
}

fmt.Printf(`Indexing Complete:
  Files: %d
  Symbols: %d
  Communities: %d
  Key Files: %v
  Dead Code: %d files
  Security Issues: %d
  Time: %v
`,
    result.TotalFiles,
    result.TotalSymbols,
    len(result.Communities),
    result.KeyFiles[:5],
    len(result.DeadCode),
    len(result.SecurityIssues),
    result.IndexTime,
)

// Use in planner
planner := planner.New(
    planner.WithCodeIndex(indexer),
    planner.WithIntelligenceLevel(planner.IntelligenceFull),
)

// Create intelligent plan
plan := planner.CreatePlan("Add authentication to the API")
for _, wave := range plan.Waves {
    fmt.Printf("Wave %d:\n", wave.Number)
    for _, task := range wave.Tasks {
        fmt.Printf("  - %s\n", task.Title)
        fmt.Printf("    Files: %v\n", task.TargetFiles)
        fmt.Printf("    Impact: %s\n", task.EstimatedImpact)
        fmt.Printf("    Risk: %s\n", task.Risk)
    }
}

// Query examples
results := indexer.Search("authentication middleware")
for _, result := range results.Files[:5] {
    fmt.Printf("%s (relevance: %.2f)\n", result.Path, result.Relevance)
}

// Get file intelligence
fileInfo := indexer.GetFileIntelligence("internal/api/router.go")
fmt.Printf(`File Intelligence:
  PageRank: %.4f
  Community: %s
  Dependencies: %d
  Dependents: %d
  Complexity: %d
  Last Modified: %v
`,
    fileInfo.PageRank,
    fileInfo.Community,
    len(fileInfo.Dependencies),
    len(fileInfo.Dependents),
    fileInfo.Complexity,
    fileInfo.LastModified,
)
```

## Testing Strategy

### Unit Tests
```go
// internal/codeindex/parser/parser_test.go
func TestParseGoFile(t *testing.T) {
    parser := NewGoParser()
    result, err := parser.Parse("testdata/sample.go")
    assert.NoError(t, err)
    assert.Equal(t, 5, len(result.Symbols))
    assert.Equal(t, 3, len(result.Imports))
}

// internal/codeindex/graph/pagerank_test.go
func TestPageRankCalculation(t *testing.T) {
    graph := NewGraph()
    // Add test nodes and edges
    graph.CalculatePageRank()
    assert.InDelta(t, 0.25, graph.GetPageRank("node1"), 0.01)
}
```

### Integration Tests
```go
// internal/codeindex/integration_test.go
func TestFullIndexingPipeline(t *testing.T) {
    indexer := NewIndexer()
    result, err := indexer.IndexRepository("testdata/repo")
    assert.NoError(t, err)
    assert.Greater(t, result.TotalFiles, 0)
    assert.Greater(t, len(result.Communities), 0)
}
```

### Benchmark Tests
```go
func BenchmarkIndexing(b *testing.B) {
    indexer := NewIndexer()
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        indexer.IndexRepository("testdata/large-repo")
    }
}
```

## Migration Path from Simple to Full

### MVP (3 days)
- Go-only parsing
- Basic import tracking
- Simple file graph
- Memory-only storage
- Integration with planner

### V1 (1 week)
- Add PageRank
- Add community detection
- SQLite persistence
- Basic search

### V2 (2 weeks)
- Multi-language support
- Dead code detection
- Git intelligence
- TUI integration

### V3 (3 weeks)
- Full DAG parity
- Web dashboard
- Documentation generation
- Vector search
- Security scanning

## Success Metrics

### Planner Improvements
- **Task Precision**: 90% of tasks target correct files
- **Dependency Accuracy**: 95% of dependencies correctly identified
- **Planning Speed**: 5x faster than without intelligence
- **Execution Success**: 30% fewer failed tasks
- **Impact Prediction**: 85% accuracy on change impact

### System Metrics
- **Indexing Coverage**: 100% of supported file types
- **Query Latency**: P99 < 200ms
- **Memory Efficiency**: < 1GB for 100k files
- **Incremental Speed**: < 1s for typical changes
- **Uptime**: 99.9% availability

## Conclusion

This comprehensive specification provides everything needed to implement DAG's full functionality within Open Agent Teams. The system will transform the planner agent from a "blind" task decomposer to an intelligent architect that truly understands the codebase structure, dependencies, and impact of changes.

Key differentiators:
1. **Deep Integration**: Not a separate tool, but core OATs functionality
2. **Go Native**: No Python dependencies, pure Go implementation
3. **Planner-First**: Every feature designed to enhance planning
4. **Real-time**: Incremental updates keep intelligence current
5. **Scalable**: From small projects to massive monorepos

The implementation will follow a phased approach, delivering value incrementally while building toward full DAG parity. The MVP can be operational in 3 days, with complete functionality achieved in 3-4 weeks.