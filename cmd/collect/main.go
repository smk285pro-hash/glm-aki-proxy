// Command collect harvests captcha device tokens from a real desktop
// Chrome session and stores them as a JSON pool for the server.
//
// Flow: open chat.z.ai -> wait for the chat box -> type a harmless
// probe so the page injects its captcha SDK -> poll the SDK's token
// minter N times -> write {"tokens": [...]}.
//
// Usage: aki-collect.exe --count 100 --out tokens.json [--headed]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mxschmitt/playwright-go"
	"glm-aki-proxy/internal/config"
	"glm-aki-proxy/internal/pool"
)

const pageURL = "https://chat.z.ai"

const desktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

func main() {
	count := flag.Int("count", 100, "how many device tokens to harvest")
	out := flag.String("out", "tokens.json", "pool file to write")
	headed := flag.Bool("headed", false, "show the browser window")
	pushRedis := flag.Bool("push-redis", false, "push harvested tokens to Redis configured in .env")
	pushOnly := flag.Bool("push-only", false, "push existing tokens from --file to Redis without browser scraping")
	file := flag.String("file", "tokens.json", "file to read when using --push-only")
	flag.Parse()

	if *pushOnly {
		cfg := config.Load()
		tokPool, err := pool.Open(cfg)
		if err != nil {
			log.Fatalf("pool init failed: %v", err)
		}
		if tokPool.Backend() == "file" {
			log.Fatalf("No Redis configured in .env! Please set UPSTASH_REDIS_REST_URL/TOKEN or REDIS_URL first.")
		}
		raw, err := os.ReadFile(*file)
		if err != nil {
			log.Fatalf("read %s: %v", *file, err)
		}
		var df struct {
			Tokens []string `json:"tokens"`
		}
		if err := json.Unmarshal(raw, &df); err != nil {
			log.Fatalf("parse %s: %v", *file, err)
		}
		if len(df.Tokens) == 0 {
			log.Printf("no tokens found in %s", *file)
			return
		}
		n, err := tokPool.Push(df.Tokens...)
		if err != nil {
			log.Fatalf("push to Redis failed: %v", err)
		}
		log.Printf("SUCCESS: pushed %d tokens to Redis (%s). Total tokens now: %d", len(df.Tokens), tokPool.Backend(), n)
		return
	}

	if *count <= 0 {
		*count = 100
	}
	if *count > 1250 {
		*count = 1250
	}
	log.Printf("harvesting %d device tokens (headed=%v, push-redis=%v)", *count, *headed, *pushRedis)

	pw, err := bootPlaywright()
	if err != nil {
		log.Fatalf("playwright: %v", err)
	}
	defer pw.Stop()

	browser, err := launch(pw, *headed)
	if err != nil {
		log.Fatalf("browser: %v", err)
	}
	defer browser.Close()

	if err := harvest(browser, *count, *out, *pushRedis); err != nil {
		log.Fatalf("harvest: %v", err)
	}
	log.Println("done")
}

// bootPlaywright starts the driver, downloading it once on first run
// (browsers themselves are never downloaded — system Chrome is used).
// It prefers the system node runtime when present.
func bootPlaywright() (*playwright.Playwright, error) {
	if pw, err := playwright.Run(); err == nil {
		return pw, nil
	}
	if os.Getenv("PLAYWRIGHT_NODEJS_PATH") == "" {
		if node, err := exec.LookPath("node"); err == nil {
			_ = os.Setenv("PLAYWRIGHT_NODEJS_PATH", node)
			if pw, err := playwright.Run(); err == nil {
				return pw, nil
			}
		}
	}
	log.Println("first run: fetching playwright driver (~40 MB, one time)...")
	if err := playwright.Install(&playwright.RunOptions{SkipInstallBrowsers: true}); err != nil {
		return nil, err
	}
	return playwright.Run()
}

// launch opens system Chrome with stealth-by-flags: the new headless
// engine (a full browser minus the window), a genuine Chrome user
// agent, and the automation flag stripped.
func launch(pw *playwright.Playwright, headed bool) (playwright.Browser, error) {
	args := []string{
		"--disable-blink-features=AutomationControlled",
		"--user-agent=" + desktopUA,
		"--window-size=1920,1080",
		"--lang=en-US",
		"--no-first-run",
		"--no-default-browser-check",
		"--password-store=basic",
		"--use-mock-keychain",
	}
	if !headed {
		args = append(args, "--headless=new")
	}
	return pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Channel:           playwright.String("chrome"),
		Headless:          playwright.Bool(false),
		Args:              args,
		IgnoreDefaultArgs: []string{"--enable-automation"},
	})
}

// harvest performs the probe flow and drains the SDK minter.
func harvest(browser playwright.Browser, total int, out string, pushRedis bool) error {
	page, err := browser.NewPage()
	if err != nil {
		return err
	}
	defer page.Close()

	log.Println("opening chat page...")
	if _, err := page.Goto(pageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	}); err != nil {
		return fmt.Errorf("goto: %w", err)
	}
	if err := page.Locator("#chat-input").WaitFor(
		playwright.LocatorWaitForOptions{Timeout: playwright.Float(20000)}); err != nil {
		return fmt.Errorf("chat box not found: %w", err)
	}
	log.Println("sending probe...")
	if err := page.Locator("#chat-input").Fill("__"); err != nil {
		return fmt.Errorf("fill: %w", err)
	}
	if err := page.Locator("#send-message-button").Click(
		playwright.LocatorClickOptions{Timeout: playwright.Float(10000)}); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	if err := waitMinter(page, 30*time.Second); err != nil {
		return err
	}

	log.Println("draining token minter...")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	type result struct {
		tokens []string
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		val, err := page.Evaluate(`async (n) => {
			const out = new Array(n);
			for (let i = 0; i < n; i++) {
				const t = window.z_um.getToken();
				out[i] = (t && typeof t.then === 'function') ? await t : t;
			}
			return out;
		}`, total)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		arr, _ := val.([]interface{})
		var toks []string
		for _, v := range arr {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				toks = append(toks, s)
			}
		}
		ch <- result{toks, nil}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("minter: %w", r.err)
		}
		if len(r.tokens) == 0 {
			return fmt.Errorf("minter returned nothing")
		}
		if err := writePool(out, r.tokens); err != nil {
			return err
		}
		if pushRedis {
			cfg := config.Load()
			tokPool, err := pool.Open(cfg)
			if err == nil && tokPool.Backend() != "file" {
				n, err := tokPool.Push(r.tokens...)
				if err != nil {
					log.Printf("WARN: failed to push to Redis: %v", err)
				} else {
					log.Printf("SUCCESS: pushed %d tokens to Redis (%s). Total tokens now: %d", len(r.tokens), tokPool.Backend(), n)
				}
			} else {
				log.Printf("WARN: --push-redis specified but no Redis config found in .env")
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("minter timed out")
	}
}

// waitMinter polls until the page exposes the token minter.
func waitMinter(page playwright.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ready, err := page.Evaluate(`typeof window.z_um !== 'undefined' && typeof window.z_um.getToken === 'function'`)
		if err == nil {
			if ok, _ := ready.(bool); ok {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("captcha SDK never appeared")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// writePool stores tokens as {"tokens": [...]} with owner-only perms.
func writePool(path string, tokens []string) error {
	raw, err := json.MarshalIndent(map[string]interface{}{"tokens": tokens}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		return err
	}
	log.Printf("saved %d tokens to %s", len(tokens), path)
	return nil
}
