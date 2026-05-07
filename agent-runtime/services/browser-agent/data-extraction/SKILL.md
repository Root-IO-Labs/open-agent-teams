# Data Extraction Skill

Extract structured data from web pages: tables, profiles, listings, and any structured content.

## When to Use

- Extracting tabular data from HTML tables
- Scraping profile information (names, titles, companies)
- Collecting product listings with prices
- Gathering any structured data from a web page

## Strategy

1. Navigate to the target page
2. Try `browser_extract` first with a JSON schema describing the desired output
   - Tier 1 (automatic): Works for HTML tables, JSON-LD, definition lists
   - Tier 2 (LLM fallback): Returns page text for you to parse
3. If `browser_extract` isn't sufficient, use `browser_get_text` and parse manually
4. For paginated data, extract from each page and combine results
5. Use `browser_find` to locate specific data sections without full snapshots

## Output Format

Return extracted data as structured JSON matching the requested schema. Include metadata like source URL and extraction timestamp.
