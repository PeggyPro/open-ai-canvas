```bash
# 使用 Git 忽略的目录保存本地开发数据和缓存
mkdir -p .local/project-workbench-debug .local/cache/go-build .local/cache/go-mod

# 终端一：后端
cd /d C:\Users\fuhan\Documents\github\open-ai-canvas\backend

set CANVAS_BACKEND_ADDR=127.0.0.1:8080
set CANVAS_BACKEND_DATA_DIR=C:\Users\fuhan\Documents\github\open-ai-canvas\.local\project-workbench-debug
set GOCACHE=C:\Users\fuhan\Documents\github\open-ai-canvas\.local\cache\go-build
set GOMODCACHE=C:\Users\fuhan\Documents\github\open-ai-canvas\.local\cache\go-mod

go env GOCACHE
go env GOMODCACHE

go run .\cmd\server

# 终端二：前端
cd ../web
bun install --frozen-lockfile
bun run dev --port 3001
```
