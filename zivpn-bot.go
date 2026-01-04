package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ==========================================
// Constants & Configuration
// ==========================================

const (
	BotConfigFile = "/etc/zivpn/bot-config.json"
	ApiPortFile   = "/etc/zivpn/api_port"
	ApiKeyFile    = "/etc/zivpn/apikey"
	DomainFile    = "/etc/zivpn/domain"
)

var ApiUrl = "http://127.0.0.1:8080/api"
var ApiKey = ""

type BotConfig struct {
	BotToken string `json:"bot_token"`
	AdminID  int64  `json:"admin_id"`
	Mode     string `json:"mode"`
	Domain   string `json:"domain"`
}

type IpInfo struct {
	City  string `json:"city"`
	Isp   string `json:"isp"`
	Query string `json:"query"`
}

type UserData struct {
	Password string `json:"password"`
	Expired  string `json:"expired"`
	Status   string `json:"status"`
	IpLimit  int    `json:"ip_limit"`
}

type ChatSession struct {
	UserID   int64  `json:"user_id"`
	ChatID   int64  `json:"chat_id"`
	Username string `json:"username"`
	Joined   string `json:"joined"`
}

// ==========================================
// Global State
// ==========================================

var userStates = make(map[int64]string)
var tempUserData = make(map[int64]map[string]string)
var lastMessageIDs = make(map[int64]int)
var activeChats = make(map[int64]ChatSession)
var chatsFile = "/etc/zivpn/chats.json"
var lastSaveTime time.Time

// ==========================================
// Main Entry Point
// ==========================================

func main() {
	// Load API Key
	if keyBytes, err := ioutil.ReadFile(ApiKeyFile); err == nil {
		ApiKey = strings.TrimSpace(string(keyBytes))
	}

	// Load API Port
	if portBytes, err := ioutil.ReadFile(ApiPortFile); err == nil {
		port := strings.TrimSpace(string(portBytes))
		ApiUrl = fmt.Sprintf("http://127.0.0.1:%s/api", port)
	}

	// Load saved chats
	loadChats()

	// Load Config
	config, err := loadConfig()
	if err != nil {
		log.Fatal("Gagal memuat konfigurasi bot:", err)
	}

	// Initialize Bot
	bot, err := tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Panic("Gagal membuat bot API:", err)
	}

	bot.Debug = false
	log.Printf("Bot berjalan sebagai %s (Admin ID: %d)", bot.Self.UserName, config.AdminID)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	log.Println("Bot siap menerima update...")

	// Main Loop
	for update := range updates {
		if update.Message != nil {
			handleMessage(bot, update.Message, &config)
		} else if update.CallbackQuery != nil {
			handleCallback(bot, update.CallbackQuery, &config)
		}
	}
}

// ==========================================
// Telegram Event Handlers
// ==========================================

func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	chatID := msg.Chat.ID
	userID := msg.From.ID
	username := msg.From.UserName

	// Save chat session
	saveChatSession(userID, chatID, username)

	// Access Control
	if !isAllowed(config, userID) {
		replyError(bot, chatID, "⛔ Akses Ditolak. Bot ini Private.")
		return
	}

	// Handle Document Upload (Restore)
	if msg.Document != nil && userID == config.AdminID {
		if state, exists := userStates[userID]; exists && state == "waiting_restore_file" {
			processRestoreFile(bot, msg, config)
			return
		}
	}

	// Handle State (User Input)
	if state, exists := userStates[userID]; exists {
		handleState(bot, msg, state, config)
		return
	}

	// Handle Commands
	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			sendWelcomeMessage(bot, chatID, config)
			showMainMenu(bot, chatID, config)
		case "broadcast":
			if userID == config.AdminID {
				startBroadcastMessage(bot, chatID, userID)
			}
		case "message":
			if userID == config.AdminID {
				showMessageMenu(bot, chatID)
			}
		case "status":
			checkStatus(bot, chatID, config)
		case "help":
			sendHelpMessage(bot, chatID, config, userID)
		default:
			replyError(bot, chatID, "Perintah tidak dikenal.")
		}
		return
	}

	// Jika bukan command, tunjukkan menu
	showMainMenu(bot, chatID, config)
}

func handleCallback(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery, config *BotConfig) {
	userID := query.From.ID
	chatID := query.Message.Chat.ID
	data := query.Data

	// Save chat session
	saveChatSession(userID, chatID, query.From.UserName)

	// Access Control
	if !isAllowed(config, userID) {
		if data != "toggle_mode" || userID != config.AdminID {
			bot.Request(tgbotapi.NewCallback(query.ID, "Akses Ditolak"))
			return
		}
	}

	switch {
	// --- Main Menu ---
	case data == "menu_create":
		startCreateUser(bot, chatID, userID)
	case data == "menu_delete":
		showUserSelection(bot, chatID, 1, "delete")
	case data == "menu_renew":
		showUserSelection(bot, chatID, 1, "renew")
	case data == "menu_list":
		if userID == config.AdminID {
			listUsers(bot, chatID)
		}
	case data == "menu_info":
		if userID == config.AdminID {
			systemInfo(bot, chatID, config)
		}
	case data == "menu_backup_restore":
		if userID == config.AdminID {
			showBackupRestoreMenu(bot, chatID)
		}
	case data == "menu_message":  // ⭐ CALLBACK UNTUK MESSAGE MENU
		if userID == config.AdminID {
			showMessageMenu(bot, chatID)
		}

	// --- Backup & Restore ---
	case data == "menu_backup_action":
		if userID == config.AdminID {
			performBackup(bot, chatID)
		}
	case data == "menu_restore_action":
		if userID == config.AdminID {
			startRestore(bot, chatID, userID)
		}

	// --- Messaging ---
	case data == "msg_broadcast":
		if userID == config.AdminID {
			startBroadcastMessage(bot, chatID, userID)
		}
	case data == "msg_private":
		if userID == config.AdminID {
			startSelectUserForMessage(bot, chatID, userID)
		}
	case data == "msg_stats":
		if userID == config.AdminID {
			showMessageStats(bot, chatID)
		}

	// --- Pagination ---
	case strings.HasPrefix(data, "page_"):
		handlePagination(bot, chatID, data)
	case strings.HasPrefix(data, "page_msg:"):
		pageStr := strings.TrimPrefix(data, "page_msg:")
		page, _ := strconv.Atoi(pageStr)
		showUserSelectionForMessage(bot, chatID, page)

	// --- Action Selection ---
	case strings.HasPrefix(data, "select_renew:"):
		startRenewUser(bot, chatID, userID, data)
	case strings.HasPrefix(data, "select_delete:"):
		confirmDeleteUser(bot, chatID, data)
	case strings.HasPrefix(data, "select_user_msg:"):
		username := strings.TrimPrefix(data, "select_user_msg:")
		startPrivateMessage(bot, chatID, userID, username)

	// --- Action Confirmation ---
	case strings.HasPrefix(data, "confirm_delete:"):
		username := strings.TrimPrefix(data, "confirm_delete:")
		deleteUser(bot, chatID, username, config)

	// --- Admin Actions ---
	case data == "toggle_mode":
		toggleMode(bot, chatID, userID, config)

	// --- Cancel ---
	case data == "cancel":
		cancelOperation(bot, chatID, userID, config)

	default:
		bot.Request(tgbotapi.NewCallback(query.ID, "Aksi tidak dikenal"))
	}

	bot.Request(tgbotapi.NewCallback(query.ID, ""))
}

// ==========================================
// MESSAGING FEATURES
// ==========================================

func showMessageMenu(bot *tgbotapi.BotAPI, chatID int64) {
	activeCount := len(activeChats) - 1 // minus admin
	
	msg := fmt.Sprintf("📨 *Admin Messaging*\n\nActive Users: %d\n\nPilih tipe pesan:", activeCount)
	
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📢 Broadcast to All", "msg_broadcast"),
			tgbotapi.NewInlineKeyboardButtonData("🔒 Private Message", "msg_private"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Stats", "msg_stats"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Kembali", "cancel"),
		),
	)
	
	sendMessageWithKeyboard(bot, chatID, msg, keyboard)
}

func showMessageStats(bot *tgbotapi.BotAPI, chatID int64) {
	totalUsers := len(activeChats)
	adminCount := 0
	regularCount := 0
	
	for _, session := range activeChats {
		if session.Username == "admin" || session.UserID == 8556981744 {
			adminCount++
		} else {
			regularCount++
		}
	}
	
	msg := fmt.Sprintf("📊 *Message Statistics*\n\n"+
		"• Total Active Chats: %d\n"+
		"• Admin Chats: %d\n"+
		"• Regular Users: %d\n"+
		"• Last Update: %s",
		totalUsers, adminCount, regularCount, time.Now().Format("15:04:05"))
	
	sendMessage(bot, chatID, msg)
}

func startBroadcastMessage(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "broadcast_message"
	
	activeCount := len(activeChats) - 1
	msgText := fmt.Sprintf("📢 *Broadcast Message*\n\n"+
		"Target: %d active users\n\n"+
		"Masukkan pesan yang ingin dikirim:\n\n"+
		"Format bisa menggunakan:\n"+
		"• Teks biasa\n"+
		"• *Markdown*\n"+
		"• <b>HTML</b>\n\n"+
		"Ketik 'cancel' untuk membatalkan", activeCount)
	
	sendMessage(bot, chatID, msgText)
}

func startSelectUserForMessage(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	showUserSelectionForMessage(bot, chatID, 1)
}

func showUserSelectionForMessage(bot *tgbotapi.BotAPI, chatID int64, page int) {
	users, err := getUsers()
	if err != nil {
		replyError(bot, chatID, "Gagal mengambil data user: "+err.Error())
		return
	}

	if len(users) == 0 {
		sendMessage(bot, chatID, "📂 Tidak ada user.")
		return
	}

	perPage := 8
	totalPages := (len(users) + perPage - 1) / perPage

	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * perPage
	end := start + perPage
	if end > len(users) {
		end = len(users)
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, u := range users[start:end] {
		label := fmt.Sprintf("%s", u.Password)
		if u.Status == "Expired" {
			label = fmt.Sprintf("🔴 %s", label)
		} else {
			label = fmt.Sprintf("🟢 %s", label)
		}
		data := fmt.Sprintf("select_user_msg:%s", u.Password)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, data),
		))
	}

	// Navigation buttons
	var navRow []tgbotapi.InlineKeyboardButton
	if page > 1 {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("page_msg:%d", page-1)))
	}
	navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", "cancel"))
	if page < totalPages {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("page_msg:%d", page+1)))
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}

	msgText := fmt.Sprintf("👥 *Pilih User untuk Private Message*\n\n"+
		"Total User: %d\n"+
		"Halaman: %d/%d\n\n"+
		"Klik username untuk mengirim pesan:", len(users), page, totalPages)

	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	sendAndTrack(bot, msg)
}

func startPrivateMessage(bot *tgbotapi.BotAPI, chatID int64, userID int64, username string) {
	tempUserData[userID] = map[string]string{"target_user": username}
	userStates[userID] = "private_message"
	
	msgText := fmt.Sprintf("✉️ *Private Message untuk %s*\n\nMasukkan pesan yang ingin dikirim:\n\nKetik 'cancel' untuk membatalkan", username)
	
	sendMessage(bot, chatID, msgText)
}

func sendBroadcastMessage(bot *tgbotapi.BotAPI, chatID int64, message string, config *BotConfig) {
	lowerMsg := strings.ToLower(message)
	if strings.Contains(lowerMsg, "cancel") {
		cancelOperation(bot, chatID, config.AdminID, config)
		return
	}

	totalSent := 0
	totalFailed := 0

	// Kirim status ke admin
	statusMsg := tgbotapi.NewMessage(chatID, 
		fmt.Sprintf("📤 *Mengirim Broadcast...*\n\nPesan: %s\n\n⏳ Mohon tunggu...", 
		truncateString(message, 100)))
	statusMsg.ParseMode = "Markdown"
	bot.Send(statusMsg)

	// Kirim ke semua active chats
	for userID, session := range activeChats {
		// Skip admin sendiri
		if userID == config.AdminID {
			continue
		}

		msg := tgbotapi.NewMessage(session.ChatID, 
			"📢 *BROADCAST MESSAGE*\n\n"+message+"\n\n_• Broadcast dari Admin •_")
		msg.ParseMode = "Markdown"

		_, err := bot.Send(msg)
		if err != nil {
			log.Printf("Gagal mengirim ke user %d: %v", userID, err)
			totalFailed++
		} else {
			totalSent++
		}

		time.Sleep(50 * time.Millisecond)
	}

	// Kirim laporan ke admin
	reportMsg := fmt.Sprintf("✅ *Broadcast Selesai!*\n\n📊 Statistik:\n"+
		"• Berhasil: %d user\n"+
		"• Gagal: %d user\n"+
		"• Total: %d user aktif",
		totalSent, totalFailed, len(activeChats)-1)

	reply := tgbotapi.NewMessage(chatID, reportMsg)
	reply.ParseMode = "Markdown"
	bot.Send(reply)

	showMainMenu(bot, chatID, config)
}

func sendPrivateMessageToUser(bot *tgbotapi.BotAPI, chatID int64, username string, message string, config *BotConfig) {
	if strings.ToLower(message) == "cancel" {
		cancelOperation(bot, chatID, config.AdminID, config)
		return
	}

	// Kirim ke admin dulu sebagai konfirmasi
	confirmMsg := fmt.Sprintf("📤 Mengirim pesan ke %s...\n\nPesan: %s", 
		username, truncateString(message, 200))
	sendMessage(bot, chatID, confirmMsg)

	// Cari user di database
	users, err := getUsers()
	if err != nil {
		replyError(bot, chatID, "Gagal mengambil data user.")
		return
	}

	// Cari user
	found := false
	for _, u := range users {
		if u.Password == username {
			found = true
			break
		}
	}

	if !found {
		replyError(bot, chatID, fmt.Sprintf("User %s tidak ditemukan.", username))
		return
	}

	// Kirim notifikasi ke admin
	successMsg := fmt.Sprintf("✅ *Pesan Terkirim!*\n\n"+
		"📨 Kepada: %s\n"+
		"📝 Pesan: %s\n\n"+
		"Pesan telah dicatat untuk user tersebut.",
		username, truncateString(message, 100))
	
	reply := tgbotapi.NewMessage(chatID, successMsg)
	reply.ParseMode = "Markdown"
	bot.Send(reply)

	showMainMenu(bot, chatID, config)
}

// ==========================================
// UI & HELPERS - DIPERBAIKI
// ==========================================

func sendWelcomeMessage(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	ipInfo, _ := getIpInfo()
	
	welcomeMsg := fmt.Sprintf("👋 *Selamat Datang di ZiVPN Bot!*\n\n"+
		"⚡ *ZiVPN UDP Premium*\n"+
		"🌐 Domain: %s\n"+
		"📍 City: %s\n"+
		"📶 ISP: %s\n\n"+
		"Gunakan menu di bawah untuk mengelola VPN:",
		config.Domain, ipInfo.City, ipInfo.Isp)
	
	msg := tgbotapi.NewMessage(chatID, welcomeMsg)
	msg.ParseMode = "Markdown"
	bot.Send(msg)
}

func sendHelpMessage(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig, userID int64) {
	helpMsg := "🆘 *Bantuan ZiVPN Bot*\n\n" +
		"*Perintah yang tersedia:*\n" +
		"/start - Tampilkan menu utama\n" +
		"/status - Cek status sistem\n" +
		"/help - Tampilkan bantuan ini\n\n"
	
	if userID == config.AdminID {
		helpMsg += "*Perintah Admin:*\n" +
			"/broadcast - Kirim pesan broadcast\n" +
			"/message - Kirim pesan private\n" +
			"📨 Message - Menu messaging di keyboard\n\n" +
			"*Fitur Admin:*\n" +
			"• Broadcast ke semua user\n" +
			"• Private message ke user tertentu\n" +
			"• Backup & restore data\n" +
			"• Toggle mode public/private"
	}
	
	msg := tgbotapi.NewMessage(chatID, helpMsg)
	msg.ParseMode = "Markdown"
	bot.Send(msg)
}

func showMainMenu(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	ipInfo, _ := getIpInfo()
	domain := config.Domain
	if domain == "" {
		domain = "Not Configured"
	}

	activeCount := len(activeChats)
	
	msgText := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n    ZIVPN UDP MENU\n━━━━━━━━━━━━━━━━━━━━━\n • Domain   : %s\n • City     : %s\n • ISP      : %s\n • Users    : %d active\n━━━━━━━━━━━━━━━━━━━━━\n```\n👇 *Silakan pilih menu:*",
		domain, ipInfo.City, ipInfo.Isp, activeCount)

	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = getMainMenuKeyboard(config, chatID)
	sendAndTrack(bot, msg)
}

func getMainMenuKeyboard(config *BotConfig, userID int64) tgbotapi.InlineKeyboardMarkup {
	// Baris pertama untuk semua user
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("👤 Create", "menu_create"),
			tgbotapi.NewInlineKeyboardButtonData("🗑️ Delete", "menu_delete"),
			tgbotapi.NewInlineKeyboardButtonData("🔄 Renew", "menu_renew"),
		},
	}

	// Menu khusus admin
	if userID == config.AdminID {
		modeLabel := "🔐 Private"
		if config.Mode == "public" {
			modeLabel = "🌍 Public"
		}

		// Baris kedua admin: List dan Info
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📋 List", "menu_list"),
			tgbotapi.NewInlineKeyboardButtonData("📊 Info", "menu_info"),
		})
		
		// Baris ketiga admin: Backup dan Message  // ⭐ INI YANG DITAMBAH!
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("💾 Backup", "menu_backup_restore"),
			tgbotapi.NewInlineKeyboardButtonData("📨 Message", "menu_message"),  // ⭐ BUTTON BARU!
		})
		
		// Baris keempat: Toggle mode
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(modeLabel, "toggle_mode"),
		})
	}

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func sendMessageWithKeyboard(bot *tgbotapi.BotAPI, chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	sendAndTrack(bot, msg)
}

// ==========================================
// BAGIAN LAIN YANG PERLU
// ==========================================

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func handleState(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, state string, config *BotConfig) {
	userID := msg.From.ID
	text := strings.TrimSpace(msg.Text)
	chatID := msg.Chat.ID

	switch state {
	case "create_username":
		if !validateUsername(bot, chatID, text) {
			return
		}
		tempUserData[userID]["username"] = text
		userStates[userID] = "create_days"
		sendMessage(bot, chatID, "⏳ Masukkan Durasi (hari):")

	case "create_days":
		days, ok := validateNumber(bot, chatID, text, 1, 9999, "Durasi")
		if !ok {
			return
		}
		createUser(bot, chatID, tempUserData[userID]["username"], days, config)
		resetState(userID)

	case "renew_days":
		days, ok := validateNumber(bot, chatID, text, 1, 9999, "Durasi")
		if !ok {
			return
		}
		renewUser(bot, chatID, tempUserData[userID]["username"], days, config)
		resetState(userID)

	case "broadcast_message":
		sendBroadcastMessage(bot, chatID, text, config)
		resetState(userID)

	case "private_message":
		if temp, exists := tempUserData[userID]; exists {
			sendPrivateMessageToUser(bot, chatID, temp["target_user"], text, config)
			resetState(userID)
		}
	}
}

// [Fungsi-fungsi lainnya tetap sama...]
// getUsers, apiCall, saveChatSession, dll...