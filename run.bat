@echo off
chcp 65001 >nul
title GLM-Aki-Proxy

echo ========================================================
echo   GLM-Aki-Proxy - Standalone Local AI Router
echo   Anthropic Messages ^& OpenAI Chat API for Claude Code / Cursor
echo ========================================================
echo.

:: 1. Check Go installation
where go >nul 2>nul
if %ERRORLEVEL% neq 0 (
    echo [ERROR] Go is not installed on this system.
    echo Please download and install Go from: https://go.dev/dl/
    pause
    exit /b 1
)

:: 2. Build binaries if missing
if not exist "glm-aki-proxy.exe" (
    echo [BUILD] Building glm-aki-proxy.exe...
    go build -o glm-aki-proxy.exe ./cmd/server
    if %ERRORLEVEL% neq 0 (
        echo [ERROR] Failed to build glm-aki-proxy.exe!
        pause
        exit /b 1
    )
    echo [OK] Built glm-aki-proxy.exe successfully!
)

if not exist "aki-collect.exe" (
    echo [BUILD] Building aki-collect.exe...
    go build -o aki-collect.exe ./cmd/collect
    if %ERRORLEVEL% neq 0 (
        echo [WARN] Failed to build aki-collect.exe, will fallback to go run.
    ) else (
        echo [OK] Built aki-collect.exe successfully!
    )
)

:: 3. Check Playwright driver on first run
if not exist "%USERPROFILE%\AppData\Local\ms-playwright-go" (
    echo [INIT] Installing headless browser driver (Playwright)...
    go run github.com/mxschmitt/playwright-go/cmd/playwright install --with-deps chromium
)

:: 4. Check tokens.json
if not exist "tokens.json" (
    echo [NOTICE] tokens.json not found. Harvesting initial 50 tokens...
    if exist "aki-collect.exe" (
        .\aki-collect.exe --count 50
    ) else (
        go run ./cmd/collect --count 50
    )
)

:: 5. Open Web UI in browser & start server
echo.
echo [START] Starting GLM Proxy at http://127.0.0.1:5084 ...
start "" http://127.0.0.1:5084

.\glm-aki-proxy.exe
pause
