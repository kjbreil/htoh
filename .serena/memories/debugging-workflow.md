# Debugging Workflow for Opti

## Backend Development Cycle

### Building the Project
```bash
make build
```
- Runs `go fmt`, `go vet`, and builds with version info
- Output: `./bin/opti` executable
- Build failures will show compilation errors that must be fixed

### Starting/Restarting the Backend

**Kill all running instances:**
```bash
pkill -f "opti -c config.toml"
```

**Start the server in background:**
```bash
./bin/opti -c config.toml
```
- Add `run_in_background=true` when using Bash tool
- Server starts on http://0.0.0.0:8181
- Includes scanner, queue processor, and web server

**Check server output:**
```bash
# Use BashOutput tool with the bash_id returned from background command
```

### Typical Restart Flow
1. Make code changes
2. `make build` - compile and verify no errors
3. `pkill -f "opti -c config.toml"` - stop old server
4. `./bin/opti -c config.toml` - start new server
5. Wait for "Web server starting on http://0.0.0.0:8181"

## Frontend Testing with Chrome DevTools

### Navigation and Testing
```javascript
// List open pages
mcp__chrome-devtools__list_pages

// Take snapshot to see current page state
mcp__chrome-devtools__take_snapshot

// Handle dialogs (like confirm prompts)
mcp__chrome-devtools__handle_dialog
- action: "accept" or "dismiss"

// Click elements by uid from snapshot
mcp__chrome-devtools__click
- uid: from snapshot (e.g., "46_12")

// Fill forms
mcp__chrome-devtools__fill
- uid: input element uid
- value: text to fill

// Take screenshots for visual verification
mcp__chrome-devtools__take_screenshot
```

### Common Testing Pattern
1. **Navigate to page**: Server auto-loads http://localhost:8181
2. **Take snapshot**: See all elements and their uids
3. **Interact**: Click buttons, fill forms using uids
4. **Verify**: Take another snapshot or screenshot
5. **Check logs**: Monitor server output for errors

### Example: Testing File Conversion
```javascript
// 1. Take snapshot to find Convert button
mcp__chrome-devtools__take_snapshot

// 2. Click the Convert button (uid from snapshot)
mcp__chrome-devtools__click
- uid: "46_12"

// 3. Handle any confirmation dialogs
mcp__chrome-devtools__handle_dialog
- action: "accept"

// 4. Switch to Queue tab to monitor progress
mcp__chrome-devtools__click
- uid: "46_3"  // Queue tab button

// 5. Wait for conversion (~5 minutes for test file)

// 6. Take final snapshot to verify updated state
mcp__chrome-devtools__take_snapshot
```

### Debugging Server-Sent Events (SSE)
The frontend uses SSE for real-time updates:
- Log messages stream via `/api/events`
- Events: `task_log`, `progress_update`, `queue_updated`, `scan_complete`
- Check browser console for SSE connection issues
- Verify broadcaster is set on queue processor

## Database Inspection
```bash
# Open database directly
sqlite3 /path/to/work/.opti_cache.db

# Common queries
SELECT * FROM files;
SELECT * FROM media_info;
SELECT * FROM queue_items;
SELECT * FROM task_logs WHERE queue_item_id = X;
```

## Key Files for Web Debugging
- `internal/web/server.go` - HTTP handlers and routing
- `internal/web/events.go` - SSE event broadcasting
- `internal/web/templates/partials/scripts.html` - Frontend JavaScript
- `internal/queue/queue.go` - Queue processing and task logs

## Common Issues

**Frontend not updating:**
- Check if `BroadcastQueueUpdated` is called after state changes
- Verify event listener exists in scripts.html
- Confirm broadcaster is set on queue processor

**Build failures:**
- Read error messages carefully
- Check for missing interface methods
- Run `go mod tidy` if dependency issues

**Server crashes:**
- Check for nil pointer dereferences
- Verify context is passed to long-running operations
- Look for database connection errors
