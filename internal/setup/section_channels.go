package setup

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

// Channel setup flag values — populated by cmd/setup_openclaw.go flags.
// These are read from the pflag set via ctx if needed.
var (
	flagTelegramToken  string
	flagTelegramUserID string
	flagWhatsAppNumber string
	flagSkipTelegram   bool
	flagSkipWhatsApp   bool
)

// channelFlags registers CLI flags for non-interactive channel setup.
func channelFlags(fs *pflag.FlagSet) {
	// Flags are registered in cmd/setup_openclaw.go on the cobra command.
	// This function exists to satisfy the Section interface.
}

// runChannels handles the channels wizard section: sets up Telegram as a
// command channel and WhatsApp as an action-only notification channel.
func runChannels(ctx *SetupContext) error {
	// Telegram setup.
	if !ctx.State.Channels.Telegram.Configured {
		if err := setupTelegram(ctx); err != nil {
			ctx.Progress.MarkFailed("channels", "telegram", err.Error())
			SaveProgress(ctx.ProgressPath, ctx.Progress)
			return err
		}
	} else {
		fmt.Printf("  ✓ Telegram already configured (@%s)\n", ctx.State.Channels.Telegram.BotUsername)
	}

	// WhatsApp setup.
	if !ctx.State.Channels.WhatsApp.Configured {
		if err := setupWhatsApp(ctx); err != nil {
			ctx.Progress.MarkFailed("channels", "whatsapp", err.Error())
			SaveProgress(ctx.ProgressPath, ctx.Progress)
			return err
		}
	} else {
		fmt.Printf("  ✓ WhatsApp already configured (%s, %s)\n", ctx.State.Channels.WhatsApp.Mode, ctx.State.Channels.WhatsApp.Number)
	}

	return nil
}

// setupTelegram walks the user through Telegram bot configuration: either
// collecting an existing bot token or guiding through BotFather creation,
// then collecting the user ID via @userinfobot.
func setupTelegram(ctx *SetupContext) error {
	fmt.Println("  Telegram Bot Setup")
	fmt.Println("  ──────────────────")

	var botToken, userID string

	if ctx.Interactive {
		reader := bufio.NewReader(os.Stdin)

		fmt.Println()
		fmt.Print("  Do you already have a Telegram bot created by BotFather? [y/N] ")
		line, _ := reader.ReadString('\n')
		hasBotAlready := strings.EqualFold(strings.TrimSpace(line), "y")

		if !hasBotAlready {
			fmt.Println()
			fmt.Println("  Creating a Telegram bot:")
			fmt.Println("  1. Open Telegram and search for @BotFather")
			fmt.Println("  2. Send /newbot")
			fmt.Println("  3. Choose a display name (e.g., \"My OpenClaw\")")
			fmt.Println("  4. Choose a username (must end in \"bot\", e.g., \"my_openclaw_bot\")")
			fmt.Println("  5. BotFather will give you a token — paste it below")
			fmt.Println()
		}

		fmt.Print("  Bot token: ")
		tokenLine, _ := reader.ReadString('\n')
		botToken = strings.TrimSpace(tokenLine)
		if botToken == "" {
			return fmt.Errorf("bot token is required")
		}

		fmt.Println()
		fmt.Println("  Now get your Telegram user ID:")
		fmt.Println("  1. Open Telegram and search for @userinfobot")
		fmt.Println("  2. Send /start — it will reply with your user ID")
		fmt.Println()
		fmt.Print("  User ID: ")
		idLine, _ := reader.ReadString('\n')
		userID = strings.TrimSpace(idLine)
		if userID == "" {
			return fmt.Errorf("user ID is required")
		}
	} else {
		// Non-interactive: read from the cobra flags via the socf package variable.
		// The values are passed through by the cmd layer.
		botToken = flagTelegramToken
		userID = flagTelegramUserID
		if botToken == "" || userID == "" {
			fmt.Println("  Skipping Telegram (no --telegram-token/--telegram-user-id provided)")
			return nil
		}
	}

	// Store credentials immediately.
	if err := ctx.Creds.Set(ctx.Profile, "telegram_bot_token", botToken); err != nil {
		return fmt.Errorf("storing Telegram bot token: %w", err)
	}
	ctx.State.Credentials.TelegramBotToken = true
	SaveState(ctx.StatePath, ctx.State)

	if err := ctx.Creds.Set(ctx.Profile, "telegram_user_id", userID); err != nil {
		return fmt.Errorf("storing Telegram user ID: %w", err)
	}
	ctx.State.Credentials.TelegramUserID = true
	SaveState(ctx.StatePath, ctx.State)

	ctx.Progress.MarkComplete("channels", "telegram_token")
	SaveProgress(ctx.ProgressPath, ctx.Progress)

	// Write Telegram channel config to OpenClaw inside the VM.
	writeCmd := fmt.Sprintf(`python3 -c "
import json, os
cfg_path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault('channels', {})['telegram'] = {
    'botToken': '%s',
    'allowedUserIds': ['%s'],
    'enabled': True
}
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
"`, botToken, userID)

	if _, err := ctx.Backend.SSHCommand(ctx.Profile, writeCmd); err != nil {
		return fmt.Errorf("writing Telegram config to VM: %w", err)
	}

	// Extract bot username from token response or ask.
	botUsername := "configured"
	if parts := strings.SplitN(botToken, ":", 2); len(parts) == 2 {
		botUsername = "bot_" + parts[0]
	}

	ctx.State.Channels.Telegram.Configured = true
	ctx.State.Channels.Telegram.BotUsername = botUsername
	SaveState(ctx.StatePath, ctx.State)

	ctx.Progress.MarkComplete("channels", "telegram_config")
	SaveProgress(ctx.ProgressPath, ctx.Progress)

	fmt.Printf("  ✓ Telegram configured (locked to user ID: %s)\n", userID)
	return nil
}

// setupWhatsApp walks the user through WhatsApp linked device pairing as an
// action-only channel. WhatsApp is configured with safe defaults that prevent
// command execution from WhatsApp contacts.
func setupWhatsApp(ctx *SetupContext) error {
	fmt.Println()
	fmt.Println("  WhatsApp Setup (action channel)")
	fmt.Println("  ───────────────────────────────")
	fmt.Println()
	fmt.Println("  WhatsApp will be configured as an action-only channel.")
	fmt.Println("  OpenClaw can send you notifications and results, but no one")
	fmt.Println("  can issue commands through WhatsApp.")

	var phoneNumber string

	if ctx.Interactive {
		reader := bufio.NewReader(os.Stdin)

		fmt.Println()
		fmt.Println("  Pair your WhatsApp account via linked device:")
		fmt.Println("  1. Open WhatsApp on your phone → Settings → Linked Devices")
		fmt.Println("  2. Tap \"Link a Device\"")
		fmt.Println("  3. Scan the QR code displayed by OpenClaw")
		fmt.Println()

		// Trigger QR code display inside the VM.
		fmt.Println("  Displaying QR code from VM...")
		qrOut, err := ctx.Backend.SSHCommand(ctx.Profile, "openclaw whatsapp pair --qr-terminal 2>&1 || echo 'QR_FAILED'")
		if err != nil || strings.Contains(qrOut, "QR_FAILED") {
			fmt.Println("  ⚠ Could not display QR code automatically.")
			fmt.Println("  Run 'openclaw whatsapp pair' inside the VM manually.")
		} else {
			fmt.Println(qrOut)
		}

		fmt.Println()
		fmt.Print("  Your WhatsApp number (for trusted sender ID): ")
		numLine, _ := reader.ReadString('\n')
		phoneNumber = strings.TrimSpace(numLine)
		if phoneNumber == "" {
			return fmt.Errorf("WhatsApp phone number is required")
		}
	} else {
		phoneNumber = flagWhatsAppNumber
		if phoneNumber == "" {
			fmt.Println("  Skipping WhatsApp (no --whatsapp-number provided)")
			return nil
		}
	}

	// Write WhatsApp channel config with safe action-only defaults.
	writeCmd := fmt.Sprintf(`python3 -c "
import json, os
cfg_path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault('channels', {})['whatsapp'] = {
    'enabled': True,
    'allowCommands': False,
    'allowedSenders': ['%s'],
    'escalationPolicy': 'deny'
}
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
"`, phoneNumber)

	if _, err := ctx.Backend.SSHCommand(ctx.Profile, writeCmd); err != nil {
		return fmt.Errorf("writing WhatsApp config to VM: %w", err)
	}

	ctx.State.Channels.WhatsApp.Configured = true
	ctx.State.Channels.WhatsApp.Mode = "action-only"
	ctx.State.Channels.WhatsApp.Number = phoneNumber
	SaveState(ctx.StatePath, ctx.State)

	ctx.Progress.MarkComplete("channels", "whatsapp")
	SaveProgress(ctx.ProgressPath, ctx.Progress)

	fmt.Println()
	fmt.Printf("  ✓ WhatsApp configured\n")
	fmt.Printf("    Mode: action-only (notifications, no commands)\n")
	fmt.Printf("    Trusted sender: %s\n", phoneNumber)
	return nil
}
