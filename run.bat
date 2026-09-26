@echo off
chcp 65001 >nul
title GLM-Aki-Proxy (Local 9Router-style)

echo ========================================================
echo   GLM-Aki-Proxy - Standalone Local AI Router
echo   Anthropic Messages ^& OpenAI Chat API for Claude Code / Cursor
echo ========================================================
echo.

:: 1. Kiem tra Go
where go >nul 2>nul
if %ERRORLEVEL% neq 0 (
    echo [ERROR] Golang chua duoc cai dat tren may.
    echo Vui long tai va cai dat Go tai: https://go.dev/dl/
    echo Sau khi cai dat xong, mo lai file nay.
    pause
    exit /b 1
)

:: 2. Bien dich binary neu chua co
if not exist "glm-aki-proxy.exe" (
    echo [BUILD] Dang bien dich glm-aki-proxy.exe...
    go build -o glm-aki-proxy.exe ./cmd/server
    if %ERRORLEVEL% neq 0 (
        echo [ERROR] Build glm-aki-proxy.exe that bai!
        pause
        exit /b 1
    )
    echo [OK] Bien dich glm-aki-proxy.exe thanh cong!
)

if not exist "aki-collect.exe" (
    echo [BUILD] Dang bien dich aki-collect.exe...
    go build -o aki-collect.exe ./cmd/collect
    if %ERRORLEVEL% neq 0 (
        echo [WARN] Build aki-collect.exe that bai, se dung fallback go run khi harvest.
    ) else (
        echo [OK] Bien dich aki-collect.exe thanh cong!
    )
)

:: 3. Kiem tra Playwright chromium driver lan dau
if not exist "%USERPROFILE%\AppData\Local\ms-playwright-go" (
    echo [INIT] Cai dat trinh duyet thu hoach token ngam (Playwright)...
    go run github.com/mxschmitt/playwright-go/cmd/playwright install --with-deps chromium
)

:: 4. Kiem tra tokens.json
if not exist "tokens.json" (
    echo [NOTICE] Chua co tokens.json. Dang tu dong thu hoach 50 token ban dau...
    if exist "aki-collect.exe" (
        .\aki-collect.exe --count 50
    ) else (
        go run ./cmd/collect --count 50
    )
)

:: 5. Mo trinh duyet vao Web Chat va khoi chay Server
echo.
echo [START] Dang khoi dong GLM Proxy tai http://127.0.0.1:5084 ...
start "" http://127.0.0.1:5084

.\glm-aki-proxy.exe
pause
