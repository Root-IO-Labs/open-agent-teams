# Agent Factory: Smart Agent Selection for Every Task

## What is the Agent Factory?

The Agent Factory is a system that automatically chooses the right specialized AI agent for your task. Instead of using a generic "worker" for everything, it analyzes what you're trying to do and selects an agent with the perfect skills for that specific job.

Think of it like this: You wouldn't ask a backend developer to design your UI, or a frontend developer to optimize your database. The Agent Factory ensures you get a frontend expert for UI work, a backend specialist for API development, and a database expert for schema design.

## How Does It Work?

### The Simple Version

1. **You describe a task** → "Create a user profile page with React"
2. **The system analyzes it** → Detects this is a frontend UI task
3. **It picks the right expert** → Selects the `frontend-developer` agent
4. **The specialist gets to work** → Agent runs with React tools and UI best practices

### Real Example

Here's what happens when you create a task:

```bash
# You type:
oat worker create "Build a responsive dashboard with charts and filters"

# Behind the scenes:
Task Analysis:
  ✓ Detected: Frontend UI development
  ✓ Framework: Dashboard components needed
  ✓ Tools needed: React, charting libraries, CSS
  
Agent Selection:
  frontend-developer: 90% match ← Selected!
  fullstack-developer: 60% match
  backend-developer: 20% match

# Result:
Created specialized agent 'frontend-developer-1234'
This agent comes with:
  • React and component expertise
  • UI/UX best practices
  • Responsive design patterns
  • Charting library knowledge
```

## Types of Specialized Agents

### 🎨 Frontend Developer
**What it does:** Creates user interfaces, components, and interactive features  
**When it's used:** Tasks mentioning "UI", "React", "Vue", "component", "design", "responsive"  
**Special skills:** React/Vue/Angular, CSS/Tailwind, responsive design, accessibility  

**Example tasks:**
- "Create a user profile page with edit functionality"
- "Build a responsive navigation menu"
- "Add a data visualization dashboard"
- "Implement a shopping cart UI"

### ⚙️ Backend Developer
**What it does:** Builds APIs, services, and server-side logic  
**When it's used:** Tasks mentioning "API", "endpoint", "server", "authentication", "database queries"  
**Special skills:** REST/GraphQL APIs, authentication, database operations, server optimization  

**Example tasks:**
- "Create REST API for user management"
- "Implement JWT authentication"
- "Build a payment processing service"
- "Add caching layer to the API"

### 🔄 Fullstack Developer
**What it does:** Handles both frontend and backend work for complete features  
**When it's used:** Tasks requiring both UI and API work  
**Special skills:** Frontend frameworks, backend services, database design, deployment  

**Example tasks:**
- "Build a complete blog feature with UI and API"
- "Create a user registration flow from frontend to database"
- "Implement real-time chat functionality"
- "Add file upload with progress tracking"

### 📱 Mobile Developer
**What it does:** Creates mobile app features and screens  
**When it's used:** Tasks mentioning "iOS", "Android", "React Native", "mobile", "app"  
**Special skills:** React Native/Flutter, mobile UI patterns, device APIs, responsive layouts  

**Example tasks:**
- "Create a mobile login screen"
- "Add push notifications to the app"
- "Build an offline-capable mobile feature"
- "Implement camera integration"

### 🗄️ Database Architect
**What it does:** Designs schemas, writes migrations, optimizes queries  
**When it's used:** Tasks mentioning "database", "schema", "migration", "query optimization"  
**Special skills:** SQL, NoSQL, schema design, indexing, query optimization  

**Example tasks:**
- "Design database schema for e-commerce"
- "Optimize slow queries in user dashboard"
- "Create migration for new features"
- "Set up database indexing strategy"

### 🧪 Test Engineer
**What it does:** Writes comprehensive tests and improves test coverage  
**When it's used:** Tasks mentioning "test", "testing", "coverage", "TDD", "unit test"  
**Special skills:** Jest, Pytest, Cypress, testing patterns, mocking, coverage tools  

**Example tasks:**
- "Write unit tests for user service"
- "Add E2E tests for checkout flow"
- "Improve test coverage to 80%"
- "Create integration tests for API"

### 🚀 DevOps Engineer
**What it does:** Handles deployment, CI/CD, infrastructure, and monitoring  
**When it's used:** Tasks mentioning "deploy", "Docker", "CI/CD", "pipeline", "monitoring"  
**Special skills:** Docker, Kubernetes, CI/CD, cloud services, monitoring tools  

**Example tasks:**
- "Set up CI/CD pipeline"
- "Dockerize the application"
- "Configure auto-deployment"
- "Add application monitoring"

### 📝 API Designer
**What it does:** Designs and documents REST/GraphQL APIs  
**When it's used:** Tasks mentioning "API design", "OpenAPI", "GraphQL schema", "endpoints"  
**Special skills:** REST principles, GraphQL, OpenAPI/Swagger, API versioning  

**Example tasks:**
- "Design REST API for product catalog"
- "Create GraphQL schema for user data"
- "Document API endpoints"
- "Plan API versioning strategy"

## Using the Agent Factory

### Basic Usage

The simplest way - just describe your task naturally:

```bash
# The system automatically picks the right agent
oat worker create "Create a React component for user comments"
# → Creates a frontend-developer agent

oat worker create "Build API endpoints for product search"
# → Creates a backend-developer agent

oat worker create "Add user authentication with login page and API"
# → Creates a fullstack-developer agent

oat worker create "Write tests for the shopping cart"
# → Creates a test-engineer agent
```

### Checking Available Agents

See what specialized agents are available:

```bash
oat factory list

Available Specialized Agents:
  • frontend-developer - Creates UIs and components
  • backend-developer - Builds APIs and services
  • fullstack-developer - Complete features end-to-end
  • mobile-developer - Mobile app development
  • database-architect - Database design and optimization
  • test-engineer - Testing and quality assurance
  • devops-engineer - Deployment and infrastructure
  • api-designer - API architecture and documentation
```

### Getting Details About an Agent

Learn more about what an agent can do:

```bash
oat factory show frontend-developer

Frontend Developer
  Purpose: Creates user interfaces and interactive components
  
  Expertise:
    • React, Vue, Angular frameworks
    • Component architecture
    • State management (Redux, Context)
    • CSS/Tailwind styling
    • Responsive design
    • Accessibility (a11y)
    
  Best for:
    • Building UI components
    • Creating responsive layouts
    • Implementing interactive features
    • Fixing UI bugs
```

## Planning Complex Projects

For bigger features that need multiple specialists:

```bash
oat plan "Build a complete e-commerce product page with reviews"

The system will create a plan like:

Step 1: Backend Foundation
  → database-architect: Design product and review schemas
  → backend-developer: Create product and review APIs
  
Step 2: Frontend Implementation  
  → frontend-developer: Build product display component
  → frontend-developer: Create review system UI
  
Step 3: Integration
  → fullstack-developer: Connect frontend to APIs
  → fullstack-developer: Add cart functionality
  
Step 4: Quality & Deployment
  → test-engineer: Write component and API tests
  → devops-engineer: Set up deployment pipeline
```

## Why Use Specialized Agents?

### Without Agent Factory (Old Way)
```bash
oat worker create "Build user profile page"
# Generic worker tries to do everything
# Might miss React best practices
# Could forget responsive design
# May not know optimal patterns
```

### With Agent Factory (New Way)
```bash
oat worker create "Build user profile page"
# Frontend-developer agent selected
# Knows React patterns
# Applies responsive design automatically
# Uses component best practices
# Includes accessibility features
```

## Common Questions

### "How does it know which agent to pick?"

The system analyzes your task description looking for:
- **Technologies**: "React" → frontend, "API" → backend, "Docker" → devops
- **Patterns**: "component" → frontend, "endpoint" → backend
- **Context**: "mobile app" → mobile, "database schema" → database
- **Domain**: "UI" → frontend, "service" → backend

### "What if I need multiple agents?"

Complex tasks automatically get split:
```bash
oat plan "Create a user dashboard with real-time data"
# Creates: backend agent for WebSocket API
# Creates: frontend agent for dashboard UI
# Creates: database agent for data structure
```

### "Can I override the selection?"

Yes! You can specify exactly which agent you want:
```bash
oat worker create "Update styles" --template frontend-developer
oat worker create "Fix the bug" --template backend-developer
```

## Examples in Action

### Frontend Task

```bash
Input: "Create a responsive product card component with image, title, price, and add to cart button"

What happens:
1. Detects: Component + Responsive = Frontend task
2. Selects: frontend-developer
3. Creates with:
   - React component structure
   - Responsive CSS/Tailwind
   - Accessibility attributes
   - Cart integration logic

Output: Complete React component with:
- ProductCard.jsx component
- Responsive styles
- Props validation
- Event handlers
- Unit tests
```

### Backend Task

```bash
Input: "Create REST API for managing user preferences"

What happens:
1. Detects: REST API + User data = Backend task
2. Selects: backend-developer
3. Creates with:
   - RESTful endpoints
   - Input validation
   - Database queries
   - Error handling

Output: Complete API with:
- GET/POST/PUT/DELETE endpoints
- Request validation
- Database integration
- API documentation
- Integration tests
```

### Fullstack Task

```bash
Input: "Add a comment system with UI and backend"

What happens:
1. Detects: UI + Backend = Fullstack task
2. Selects: fullstack-developer
3. Creates:
   - Comment database schema
   - CRUD API endpoints
   - React comment components
   - Real-time updates

Output: Complete feature with:
- Database migrations
- Backend API
- Frontend components
- WebSocket integration
- End-to-end tests
```

## Tips for Best Results

### Be Specific About Technology

**Good examples:**
- ✅ "Create a React component for user avatars"
- ✅ "Build REST API with Node.js and Express"
- ✅ "Write Jest tests for the auth service"

**Less effective:**
- ❌ "Make a component"
- ❌ "Create an API"
- ❌ "Add tests"

### Include Context

The more context you provide, the better:

- **Instead of:** "Fix the form"
- **Try:** "Fix the React registration form validation"

- **Instead of:** "Make it faster"  
- **Try:** "Optimize the Node.js API response time"

### Mention the Stack

Help the system choose by mentioning your tech:

- "Create a Vue.js dashboard" (not just "dashboard")
- "Build Python Flask API" (not just "API")  
- "Write Cypress E2E tests" (not just "tests")

## Getting Started

### Step 1: Enable the Factory

```bash
# Turn on smart agent selection
export OAT_FACTORY_ENABLED=true
```

### Step 2: Try It Out

```bash
# Create a frontend task
oat worker create "Build a React sidebar navigation component"

# Create a backend task
oat worker create "Create API endpoint for file uploads"

# Create a fullstack task
oat worker create "Build a complete todo list feature with UI and API"
```

### Step 3: Watch It Work

The system will:
1. Analyze your task
2. Show you which specialized agent was selected
3. List the agent's expertise
4. Start building with the right tools and patterns

## Advanced Features

### Interactive Mode

Let the system suggest options and you pick:

```bash
export OAT_INTERACTIVE=true
oat worker create "Improve the application"

Suggested agents for your task:
1. frontend-developer (40% match) - UI improvements
2. backend-developer (35% match) - API improvements  
3. fullstack-developer (25% match) - General improvements

Choose (1-3): _
```

### Team Composition

See what specialists are working on your project:

```bash
oat factory team

Active Specialist Team:
  frontend-developer-123
    Task: Building user profile UI
    Status: Creating components...
    
  backend-developer-456
    Task: User API endpoints
    Status: Writing controllers...
    
  test-engineer-789
    Task: Writing test suite
    Status: Adding unit tests...
```

### Force Specific Agent

When you know exactly what you need:

```bash
# Force frontend agent even for general task
oat worker create "Update the feature" --template frontend-developer

# Force backend agent for debugging
oat worker create "Fix the issue" --template backend-developer
```

## The Bottom Line

The Agent Factory makes your development faster and better by matching each task with the right specialist. Frontend work goes to frontend experts, backend work to backend experts, and complex features get the full team treatment.

You get better code, proper patterns, and the right tools - automatically. Just describe what you need to build, and the system ensures the right expert handles it.