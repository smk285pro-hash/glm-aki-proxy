# GLM-Aki-Proxy — Universal Claude Code CLI (clglm) 1-Line Setup (PowerShell)
$Key = "__KEY__"
$Origin = "__ORIGIN__"

if ($Key -eq "__KEY__" -or -not $Key) { $Key = "aki-local-key" }
if ($Origin -eq "__ORIGIN__" -or -not $Origin) { $Origin = "http://127.0.0.1:5084" }
if (-not $Origin.EndsWith("/")) { $Origin = "$Origin/" }

$configDir = "$HOME\.claude-glm"
$settingsPath = "$configDir\settings.json"

if (!(Test-Path $configDir)) {
  New-Item -ItemType Directory -Force -Path $configDir | Out-Null
}

# Preserve skills, agents, hooks from ~/.claude if available
foreach ($sub in @("skills", "agents", "hooks")) {
  $src = "$HOME\.claude\$sub"
  $dst = "$configDir\$sub"
  if ((Test-Path $src) -and !(Test-Path $dst)) {
    Copy-Item -Recurse -Force -Path $src -Destination $dst -ErrorAction SilentlyContinue
  }
}

$envMap = @{}
if (Test-Path $settingsPath) {
  try {
    $raw = Get-Content $settingsPath -Raw -ErrorAction SilentlyContinue
    if ($raw -and $raw.Trim()) {
      $jsonObj = $raw | ConvertFrom-Json
      if ($jsonObj -and ($jsonObj.PSObject.Properties.Match('env').Count -gt 0) -and $jsonObj.env) {
        $jsonObj.env.PSObject.Properties | ForEach-Object { $envMap[$_.Name] = $_.Value }
      }
    }
  } catch {
    $envMap = @{}
  }
}

$envMap["ANTHROPIC_BASE_URL"] = $Origin
$envMap["ANTHROPIC_AUTH_TOKEN"] = $Key
$envMap["ANTHROPIC_DEFAULT_OPUS_MODEL"] = "claude-3-7-sonnet-20250219"
$envMap["ANTHROPIC_DEFAULT_SONNET_MODEL"] = "claude-3-7-sonnet-20250219"
$envMap["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = "glm-4.7"
$envMap["CLAUDE_CODE_SUBAGENT_MODEL"] = "claude-3-7-sonnet-20250219"
$envMap["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
$envMap["CLAUDE_CODE_DISABLE_EXPLORE_INHERIT_CAP"] = "1"
$envMap["CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT"] = "1"
$envMap["CLAUDE_CODE_ENABLE_TELEMETRY"] = "0"
$envMap["DISABLE_AUTOUPDATER"] = "1"
$envMap["CLAUDE_CODE_DISABLE_ADVISOR_TOOL"] = "1"

$cfg = @{
  env = $envMap
  model = "claude-3-7-sonnet-20250219"
  effortLevel = "high"
  maxEffortLevel = "high"
  autoCompactEnabled = $true
  autoCompactWindow = 300000
  skipDangerousModePermissionPrompt = $true
  hasCompletedOnboarding = $true
  permissions = @{
    allow = @(
      "Bash(python3 ~/.claude/skills/*)",
      "Bash(python3 ~/.aki/akidevrule/agskills/*)",
      "Read(//$HOME\.aki\akidevrule/**)"
    )
    additionalDirectories = @(
      "$HOME\.aki\akidevrule"
    )
  }
  skillOverrides = @{
    akirule = "on"
    akihelp = "name-only"
  }
  modelPicker = @{
    replaceBuiltInOptions = $true
    options = @(
      @{ model = "claude-3-7-sonnet-20250219"; label = "Claude Opus 4.8 / 3.7 Sonnet (Thinking)"; description = "Z.AI GLM 5.3 · Flagship deep reasoning & code architecture (Standard)" },
      @{ model = "claude-opus-4-8"; label = "Claude Opus 4.8"; description = "Z.AI GLM 5.3 · Flagship deep reasoning & code architecture" },
      @{ model = "claude-opus-gl-5.3"; label = "Claude Opus GL-5.3"; description = "Z.AI GLM 5.3 · Direct GLM 5.3 mapping" },
      @{ model = "claude-sonnet-4-6"; label = "Claude Sonnet 4.6"; description = "Z.AI GLM 5.2 · Fast coding with Low/Med/High/Max effort selector" },
      @{ model = "claude-sonnet-gl-5.2"; label = "Claude Sonnet GL-5.2"; description = "Z.AI GLM 5.2 · Fast, smart everyday coding & agent tasks" },
      @{ model = "claude-haiku-gl-4.7"; label = "Claude Haiku GL-4.7"; description = "Z.AI GLM 4.7 · High speed & lightweight tool execution" },
      @{ model = "claude-sonnet-gl-5-turbo"; label = "Claude Sonnet GL-5-Turbo"; description = "Z.AI GLM 5 Turbo · Ultra-low latency inference" },
      @{ model = "claude-sonnet-gl-5v-turbo"; label = "Claude Sonnet GL-5v-Turbo"; description = "Z.AI GLM 5v Turbo · Multimodal visual & code understanding" }
    )
  }
  modelSettings = @{
    "claude-3-7-sonnet-20250219" = @{ effortLevel = "high"; maxEffortLevel = "high" }
    "claude-opus-4-8" = @{ effortLevel = "high"; maxEffortLevel = "high" }
    "claude-opus-gl-5.3" = @{ effortLevel = "high"; maxEffortLevel = "high" }
    "claude-sonnet-gl-5.2" = @{ effortLevel = "high"; maxEffortLevel = "high" }
    "claude-haiku-gl-4.7" = @{ effortLevel = "high"; maxEffortLevel = "high" }
  }
}

$cfg | ConvertTo-Json -Depth 10 | Set-Content -Path $settingsPath -Encoding UTF8

if ($PROFILE) {
  $profileDir = Split-Path -Parent $PROFILE
  if (!(Test-Path $profileDir)) { New-Item -ItemType Directory -Force -Path $profileDir | Out-Null }
  if (!(Test-Path $PROFILE)) { New-Item -ItemType File -Force -Path $PROFILE | Out-Null }
  $profileContent = Get-Content $PROFILE -Raw -ErrorAction SilentlyContinue
  $glmPattern = '(?ms)^# >>> GLM Aki Proxy clglm >>>\r?\n.*?^# <<< GLM Aki Proxy clglm <<<\r?\n?'
  $line1 = '# >>> GLM Aki Proxy clglm >>>'
  $line2 = 'function clglm { $env:CLAUDE_CONFIG_DIR = "$HOME\.claude-glm"; $env:ANTHROPIC_API_KEY = $null; claude --dangerously-skip-permissions $args }'
  $line3 = '# <<< GLM Aki Proxy clglm <<<'
  $managedBlock = "$line1`n$line2`n$line3"
  Set-Content -Path $PROFILE -Value ($profileContent.TrimEnd() + "`n`n" + $managedBlock) -Encoding UTF8
}

function global:clglm { $env:CLAUDE_CONFIG_DIR = "$HOME\.claude-glm"; $env:ANTHROPIC_API_KEY = $null; claude --dangerously-skip-permissions $args }

Write-Host "========================================================" -ForegroundColor Cyan
Write-Host " [OK] GLM-Aki-Proxy Claude Code CLI (clglm) configured successfully!" -ForegroundColor Green
Write-Host " [*] Proxy: $Origin"
Write-Host " [*] Settings: $settingsPath"
Write-Host " [>] Ready to code: Run 'clglm' in any project to start" -ForegroundColor Yellow
Write-Host "========================================================" -ForegroundColor Cyan
