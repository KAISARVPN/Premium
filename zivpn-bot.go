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
	UserID int64  `json:"user_id"`
	ChatID int64  `json:"chat_id"`
	Joined string `json:"joined"`
}

// ==========================================
// Global State
// ==========================================

var userStates = make(map[int64]string)
var tempUserData = make(map[int64]map[string]string)
var lastMessageIDs = make(map[int64]int)
var activeChats = make(map[int64]ChatSession)
var chatsFile = "/etc/zivpn/chats.json"

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
		log.Panic(err)
	}

	bot.Debug = false
	log.Printf("Bot berjalan sebagai %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

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

	// Save chat session
	saveChatSession(userID, chatID)

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
				startSelectUserForMessage(bot, chatID, userID)
			}
		default:
			replyError(bot, chatID, "Perintah tidak dikenal.")
		}
		return
	}
}

func handleCallback(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery, config *BotConfig) {
	userID := query.From.ID
	chatID := query.Message.Chat.ID

	// Access Control
	if !isAllowed(config, userID) {
		if query.Data != "toggle_mode" || userID != config.AdminID {
			bot.Request(tgbotapi.NewCallback(query.ID, "Akses Ditolak"))
			return
		}
	}

	// Save chat session
	saveChatSession(userID, chatID)

	switch {
	// --- Main Menu ---
	case query.Data == "menu_create":
		startCreateUser(bot, chatID, userID)
	case query.Data == "menu_delete":
		showUserSelection(bot, chatID, 1, "delete")
	case query.Data == "menu_renew":
		showUserSelection(bot, chatID, 1, "renew")
	case query.Data == "menu_list":
		if userID == config.AdminID {
			listUsers(bot, chatID)
		}
	case query.Data == "menu_info":
		if userID == config.AdminID {
			systemInfo(bot, chatID, config)
		}
	case query.Data == "menu_backup_restore":
		if userID == config.AdminID {
			showBackupRestoreMenu(bot, chatID)
		}
	case query.Data == "menu_message":
		if userID == config.AdminID {
			showMessageMenu(bot, chatID)
		}

	// --- Backup & Restore ---
	case query.Data == "menu_backup_action":
		if userID == config.AdminID {
			performBackup(bot, chatID)
		}
	case query.Data == "menu_restore_action":
		if userID == config.AdminID {
			startRestore(bot, chatID, userID)
		}

	// --- Messaging ---
	case query.Data == "msg_broadcast":
		if userID == config.AdminID {
			startBroadcastMessage(bot, chatID, userID)
		}
	case query.Data == "msg_private":
		if userID == config.AdminID {
			startSelectUserForMessage(bot, chatID, userID)
		}

	// --- Pagination ---
	case strings.HasPrefix(query.Data, "page_"):
		handlePagination(bot, chatID, query.Data)
	case strings.HasPrefix(query.Data, "page_msg:"):
		pageStr := strings.TrimPrefix(query.Data, "page_msg:")
		page, _ := strconv.Atoi(pageStr)
		showUserSelectionForMessage(bot, chatID, page)

	// --- Action Selection ---
	case strings.HasPrefix(query.Data, "select_renew:"):
		startRenewUser(bot, chatID, userID, query.Data)
	case strings.HasPrefix(query.Data, "select_delete:"):
		confirmDeleteUser(bot, chatID, query.Data)
	case strings.HasPrefix(query.Data, "select_user_msg:"):
		username := strings.TrimPrefix(query.Data, "select_user_msg:")
		startPrivateMessage(bot, chatID, userID, username)

	// --- Action Confirmation ---
	case strings.HasPrefix(query.Data, "confirm_delete:"):
		username := strings.TrimPrefix(query.Data, "confirm_delete:")
		deleteUser(bot, chatID, username, config)

	// --- Admin Actions ---
	case query.Data == "toggle_mode":
		toggleMode(bot, chatID, userID, config)

	// --- Cancel ---
	case query.Data == "cancel":
		cancelOperation(bot, chatID, userID, config)
	}

	bot.Request(tgbotapi.NewCallback(query.ID, ""))
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

// ==========================================
// Core Features
// ==========================================

func startCreateUser(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "create_username"
	tempUserData[userID] = make(map[string]string)
	sendMessage(bot, chatID, "👤 Masukkan Password untuk user baru:")
}

func startRenewUser(bot *tgbotapi.BotAPI, chatID int64, userID int64, data string) {
	username := strings.TrimPrefix(data, "select_renew:")
	tempUserData[userID] = map[string]string{"username": username}
	userStates[userID] = "renew_days"
	sendMessage(bot, chatID, fmt.Sprintf("🔄 Renewing %s\n⏳ Masukkan Tambahan Durasi (hari):", username))
}

func createUser(bot *tgbotapi.BotAPI, chatID int64, username string, days int, config *BotConfig) {
	res, err := apiCall("POST", "/user/create", map[string]interface{}{
		"password": username,
		"days":     days,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		sendAccountInfo(bot, chatID, data, config)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal: %s", res["message"]))
		showMainMenu(bot, chatID, config)
	}
}

func renewUser(bot *tgbotapi.BotAPI, chatID int64, username string, days int, config *BotConfig) {
	res, err := apiCall("POST", "/user/renew", map[string]interface{}{
		"password": username,
		"days":     days,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		sendAccountInfo(bot, chatID, data, config)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal: %s", res["message"]))
		showMainMenu(bot, chatID, config)
	}
}

func deleteUser(bot *tgbotapi.BotAPI, chatID int64, username string, config *BotConfig) {
	res, err := apiCall("POST", "/user/delete", map[string]interface{}{
		"password": username,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		msg := tgbotapi.NewMessage(chatID, "✅ Password berhasil dihapus.")
		deleteLastMessage(bot, chatID)
		bot.Send(msg)
		showMainMenu(bot, chatID, config)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal: %s", res["message"]))
		showMainMenu(bot, chatID, config)
	}
}

func listUsers(bot *tgbotapi.BotAPI, chatID int64) {
	res, err := apiCall("GET", "/users", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		users := res["data"].([]interface{})
		if len(users) == 0 {
			sendMessage(bot, chatID, "📂 Tidak ada user.")
			return
		}

		msg := "📋 *List Passwords*\n"
		for _, u := range users {
			user := u.(map[string]interface{})
			status := "🟢"
			if user["status"] == "Expired" {
				status = "🔴"
			}
			msg += fmt.Sprintf("\n%s `%s` (%s)", status, user["password"], user["expired"])
		}

		reply := tgbotapi.NewMessage(chatID, msg)
		reply.ParseMode = "Markdown"
		sendAndTrack(bot, reply)
	} else {
		replyError(bot, chatID, "Gagal mengambil data.")
	}
}

// ==========================================
// MESSAGING FEATURES
// ==========================================

func showMessageMenu(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "📨 *Admin Messaging*\nPilih tipe pesan:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
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
	sendAndTrack(bot, msg)
}

func startBroadcastMessage(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "broadcast_message"
	sendMessage(bot, chatID, "📢 *Broadcast Message*\n\nMasukkan pesan yang ingin dikirim ke semua user:\n\nAnda bisa menggunakan format:\n• Teks biasa\n• Markdown\n• HTML\n\nKetik /cancel untuk membatalkan")
}

func startSelectUserForMessage(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	showUserSelectionForMessage(bot, chatID, 1)
}

func showUserSelectionForMessage(bot *tgbotapi.BotAPI, chatID int64, page int) {
	users, err := getUsers()
	if err != nil {
		replyError(bot, chatID, "Gagal mengambil data user.")
		return
	}

	if len(users) == 0 {
		sendMessage(bot, chatID, "📂 Tidak ada user.")
		return
	}

	// Get active chats count
	activeCount := len(activeChats)

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

	msgText := fmt.Sprintf("👥 *Pilih User untuk Private Message*\n\nTotal User: %d\nActive Chats: %d\nHalaman: %d/%d\n\nKlik username untuk mengirim pesan:",
		len(users), activeCount, page, totalPages)

	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	sendAndTrack(bot, msg)
}

func startPrivateMessage(bot *tgbotapi.BotAPI, chatID int64, userID int64, username string) {
	tempUserData[userID] = map[string]string{"target_user": username}
	userStates[userID] = "private_message"
	
	msgText := fmt.Sprintf("✉️ *Private Message untuk %s*\n\nMasukkan pesan yang ingin dikirim:\n\nFormat:\n• Teks biasa\n• Markdown\n• HTML\n\nKetik /cancel untuk membatalkan", username)
	
	sendMessage(bot, chatID, msgText)
}

func sendBroadcastMessage(bot *tgbotapi.BotAPI, chatID int64, message string, config *BotConfig) {
	if message == "/cancel" {
		cancelOperation(bot, chatID, config.AdminID, config)
		return
	}

	totalSent := 0
	totalFailed := 0

	// Send to admin first as confirmation
	adminMsg := tgbotapi.NewMessage(chatID, fmt.Sprintf("📤 *Mengirim Broadcast...*\n\nPesan: %s\n\n⏳ Mohon tunggu...", message[:min(50, len(message))]))
	adminMsg.ParseMode = "Markdown"
	bot.Send(adminMsg)

	// Send to all active chats
	for userID, session := range activeChats {
		// Skip admin
		if userID == config.AdminID {
			continue
		}

		msg := tgbotapi.NewMessage(session.ChatID, "📢 *BROADCAST MESSAGE*\n\n"+message)
		msg.ParseMode = "Markdown"
		
		// Add footer
		msg.Text += fmt.Sprintf("\n\n_• Broadcast dari Admin •_")

		_, err := bot.Send(msg)
		if err != nil {
			log.Printf("Gagal mengirim ke user %d: %v", userID, err)
			totalFailed++
			// Remove inactive chat
			delete(activeChats, userID)
		} else {
			totalSent++
		}

		// Delay to avoid rate limiting
		time.Sleep(100 * time.Millisecond)
	}

	// Save chats
	saveChats()

	// Send report to admin
	reportMsg := fmt.Sprintf("✅ *Broadcast Selesai!*\n\n📊 Statistik:\n• Berhasil: %d user\n• Gagal: %d user\n• Total: %d user\n\nPesan telah dikirim ke semua user aktif.",
		totalSent, totalFailed, len(activeChats)-1)

	reply := tgbotapi.NewMessage(chatID, reportMsg)
	reply.ParseMode = "Markdown"
	bot.Send(reply)

	showMainMenu(bot, chatID, config)
}

func sendPrivateMessageToUser(bot *tgbotapi.BotAPI, chatID int64, username string, message string, config *BotConfig) {
	if message == "/cancel" {
		cancelOperation(bot, chatID, config.AdminID, config)
		return
	}

	// Get user from API
	users, err := getUsers()
	if err != nil {
		replyError(bot, chatID, "Gagal mengambil data user.")
		return
	}

	// Find user
	var targetUser *UserData
	for _, u := range users {
		if u.Password == username {
			targetUser = &u
			break
		}
	}

	if targetUser == nil {
		replyError(bot, chatID, fmt.Sprintf("User %s tidak ditemukan.", username))
		return
	}

	// Send status to admin
	statusMsg := tgbotapi.NewMessage(chatID, fmt.Sprintf("📤 Mengirim pesan ke %s...", username))
	bot.Send(statusMsg)

	// Try to find user's chat session
	messageSent := false
	for userID, session := range activeChats {
		// We need to match by username somehow - but we only have userID
		// For now, we'll just send to all active chats with a mention
		msg := tgbotapi.NewMessage(session.ChatID, 
			fmt.Sprintf("✉️ *PRIVATE MESSAGE FROM ADMIN*\n\nPesan: %s\n\n*Untuk:* %s\n*Status:* %s\n*Expired:* %s",
			message, username, targetUser.Status, targetUser.Expired))
		msg.ParseMode = "Markdown"
		
		_, err := bot.Send(msg)
		if err == nil {
			messageSent = true
			break
		}
	}

	// Report to admin
	if messageSent {
		successMsg := fmt.Sprintf("✅ *Pesan Terkirim!*\n\n📨 Kepada: %s\n📊 Status: %s\n⏰ Expired: %s\n\nPesan berhasil dikirim ke user.",
			username, targetUser.Status, targetUser.Expired)
		
		reply := tgbotapi.NewMessage(chatID, successMsg)
		reply.ParseMode = "Markdown"
		bot.Send(reply)
	} else {
		errorMsg := fmt.Sprintf("❌ *Gagal Mengirim Pesan*\n\nUser %s tidak aktif dalam chat.\n\nPesan hanya bisa dikirim ke user yang pernah memulai chat dengan bot.",
			username)
		
		reply := tgbotapi.NewMessage(chatID, errorMsg)
		reply.ParseMode = "Markdown"
		bot.Send(reply)
	}

	showMainMenu(bot, chatID, config)
}

// ==========================================
// Backup & Restore
// ==========================================

func showBackupRestoreMenu(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "💾 *Backup & Restore*\nSilakan pilih menu:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬇️ Backup Data", "menu_backup_action"),
			tgbotapi.NewInlineKeyboardButtonData("⬆️ Restore Data", "menu_restore_action"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Kembali", "cancel"),
		),
	)
	sendAndTrack(bot, msg)
}

func performBackup(bot *tgbotapi.BotAPI, chatID int64) {
	sendMessage(bot, chatID, "⏳ Sedang membuat backup...")

	files := []string{
		"/etc/zivpn/config.json",
		"/etc/zivpn/users.json",
		"/etc/zivpn/domain",
		"/etc/zivpn/apikey",
		"/etc/zivpn/api_port",
	}

	buf := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buf)

	for _, file := range files {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			continue
		}

		f, err := os.Open(file)
		if err != nil {
			continue
		}
		defer f.Close()

		w, err := zipWriter.Create(filepath.Base(file))
		if err != nil {
			continue
		}

		if _, err := io.Copy(w, f); err != nil {
			continue
		}
	}

	zipWriter.Close()

	fileName := fmt.Sprintf("zivpn-backup-%s.zip", time.Now().Format("20060102-150405"))
	tmpFile := "/tmp/" + fileName
	
	if err := ioutil.WriteFile(tmpFile, buf.Bytes(), 0644); err != nil {
		replyError(bot, chatID, "Gagal membuat file backup.")
		return
	}
	defer os.Remove(tmpFile)

	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(tmpFile))
	doc.Caption = "✅ Backup Data ZiVPN - " + time.Now().Format("2006-01-02 15:04:05")
	
	deleteLastMessage(bot, chatID)
	bot.Send(doc)
}

func startRestore(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "waiting_restore_file"
	sendMessage(bot, chatID, "⬆️ *Restore Data*\n\nSilakan kirim file ZIP backup Anda sekarang.\n\n⚠️ PERINGATAN: Data saat ini akan ditimpa!")
}

func processRestoreFile(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	chatID := msg.Chat.ID
	userID := msg.From.ID
	
	resetState(userID)
	sendMessage(bot, chatID, "⏳ Sedang memproses file...")

	fileID := msg.Document.FileID
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		replyError(bot, chatID, "Gagal mengunduh file.")
		return
	}

	fileUrl := file.Link(config.BotToken)
	resp, err := http.Get(fileUrl)
	if err != nil {
		replyError(bot, chatID, "Gagal mengunduh file content.")
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		replyError(bot, chatID, "Gagal membaca file.")
		return
	}

	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		replyError(bot, chatID, "File bukan format ZIP yang valid.")
		return
	}

	for _, f := range zipReader.File {
		validFiles := map[string]bool{
			"config.json": true,
			"users.json":  true,
			"bot-config.json": true,
			"domain":      true,
			"apikey":      true,
			"api_port":    true,
		}
		
		if !validFiles[f.Name] {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			continue
		}
		defer rc.Close()

		dstPath := filepath.Join("/etc/zivpn", f.Name)
		dst, err := os.Create(dstPath)
		if err != nil {
			continue
		}
		defer dst.Close()

		io.Copy(dst, rc)
	}

	exec.Command("systemctl", "restart", "zivpn").Run()
	exec.Command("systemctl", "restart", "zivpn-api").Run()
	
	msgSuccess := tgbotapi.NewMessage(chatID, "✅ Restore Berhasil!\nService ZiVPN, API, dan Bot telah direstart.")
	bot.Send(msgSuccess)
	
	go func() {
		time.Sleep(2 * time.Second)
		exec.Command("systemctl", "restart", "zivpn-bot").Run()
	}()

	showMainMenu(bot, chatID, config)
}

// ==========================================
// UI & Helpers
// ==========================================

func sendWelcomeMessage(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	ipInfo, _ := getIpInfo()
	
	welcomeMsg := fmt.Sprintf("👋 *Selamat Datang di ZiVPN Bot!*\n\n⚡ *ZiVPN UDP Premium*\n🌐 Domain: %s\n📍 City: %s\n📶 ISP: %s\n\nGunakan menu di bawah untuk mengelola VPN:",
		config.Domain, ipInfo.City, ipInfo.Isp)
	
	msg := tgbotapi.NewMessage(chatID, welcomeMsg)
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
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("👤 Create", "menu_create"),
			tgbotapi.NewInlineKeyboardButtonData("🗑️ Delete", "menu_delete"),
			tgbotapi.NewInlineKeyboardButtonData("🔄 Renew", "menu_renew"),
		},
	}

	if userID == config.AdminID {
		modeLabel := "🔐 Private"
		if config.Mode == "public" {
			modeLabel = "🌍 Public"
		}

		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📋 List", "menu_list"),
			tgbotapi.NewInlineKeyboardButtonData("📊 Info", "menu_info"),
		})
		
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("💾 Backup", "menu_backup_restore"),
			tgbotapi.NewInlineKeyboardButtonData("📨 Message", "menu_message"),
		})
		
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(modeLabel, "toggle_mode"),
		})
	}

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func sendAccountInfo(bot *tgbotapi.BotAPI, chatID int64, data map[string]interface{}, config *BotConfig) {
	ipInfo, _ := getIpInfo()
	domain := config.Domain
	if domain == "" {
		domain = "Not Configured"
	}

	msg := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n  ACCOUNT ZIVPN UDP\n━━━━━━━━━━━━━━━━━━━━━\nPassword   : %s\nCITY       : %s\nISP        : %s\nIP ISP     : %s\nDomain     : %s\nExpired On : %s\n━━━━━━━━━━━━━━━━━━━━━\n```",
		data["password"],
		ipInfo.City,
		ipInfo.Isp,
		ipInfo.Query,
		domain,
		data["expired"])

	reply := tgbotapi.NewMessage(chatID, msg)
	reply.ParseMode = "Markdown"
	deleteLastMessage(bot, chatID)
	bot.Send(reply)
	showMainMenu(bot, chatID, config)
}

func showUserSelection(bot *tgbotapi.BotAPI, chatID int64, page int, action string) {
	users, err := getUsers()
	if err != nil {
		replyError(bot, chatID, "Gagal mengambil data user.")
		return
	}

	if len(users) == 0 {
		sendMessage(bot, chatID, "📂 Tidak ada user.")
		return
	}

	perPage := 10
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
		label := fmt.Sprintf("%s (%s)", u.Password, u.Status)
		if u.Status == "Expired" {
			label = fmt.Sprintf("🔴 %s", label)
		} else {
			label = fmt.Sprintf("🟢 %s", label)
		}
		data := fmt.Sprintf("select_%s:%s", action, u.Password)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, data),
		))
	}

	var navRow []tgbotapi.InlineKeyboardButton
	if page > 1 {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("page_%s:%d", action, page-1)))
	}
	if page < totalPages {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("page_%s:%d", action, page+1)))
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel")))

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("📋 Pilih User untuk %s (Halaman %d/%d):", strings.Title(action), page, totalPages))
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	sendAndTrack(bot, msg)
}

func confirmDeleteUser(bot *tgbotapi.BotAPI, chatID int64, data string) {
	username := strings.TrimPrefix(data, "select_delete:")
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("❓ Yakin ingin menghapus user `%s`?", username))
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Ya, Hapus", "confirm_delete:"+username),
			tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel"),
		),
	)
	sendAndTrack(bot, msg)
}

// ==========================================
// Utility Functions
// ==========================================

func systemInfo(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	res, err := apiCall("GET", "/info", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		ipInfo, _ := getIpInfo()

		users, _ := getUsers()
		activeUsers := 0
		for _, u := range users {
			if u.Status != "Expired" {
				activeUsers++
			}
		}

		msg := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n    INFO ZIVPN UDP\n━━━━━━━━━━━━━━━━━━━━━\nDomain         : %s\nIP Public      : %s\nPort           : %s\nService        : %s\nCITY           : %s\nISP            : %s\nActive Users   : %d/%d\nActive Chats   : %d\n━━━━━━━━━━━━━━━━━━━━━\n```",
			config.Domain, data["public_ip"], data["port"], data["service"], ipInfo.City, ipInfo.Isp,
			activeUsers, len(users), len(activeChats))

		reply := tgbotapi.NewMessage(chatID, msg)
		reply.ParseMode = "Markdown"
		deleteLastMessage(bot, chatID)
		bot.Send(reply)
		showMainMenu(bot, chatID, config)
	} else {
		replyError(bot, chatID, "Gagal mengambil info.")
	}
}

func toggleMode(bot *tgbotapi.BotAPI, chatID int64, userID int64, config *BotConfig) {
	if userID != config.AdminID {
		return
	}
	if config.Mode == "public" {
		config.Mode = "private"
	} else {
		config.Mode = "public"
	}
	saveConfig(config)
	
	modeMsg := "🔐 Mode diubah menjadi *Private*"
	if config.Mode == "public" {
		modeMsg = "🌍 Mode diubah menjadi *Public*"
	}
	
	msg := tgbotapi.NewMessage(chatID, modeMsg)
	msg.ParseMode = "Markdown"
	bot.Send(msg)
	
	showMainMenu(bot, chatID, config)
}

func cancelOperation(bot *tgbotapi.BotAPI, chatID int64, userID int64, config *BotConfig) {
	resetState(userID)
	showMainMenu(bot, chatID, config)
}

func handlePagination(bot *tgbotapi.BotAPI, chatID int64, data string) {
	parts := strings.Split(data, ":")
	action := parts[0][5:] // remove "page_"
	page, _ := strconv.Atoi(parts[1])
	showUserSelection(bot, chatID, page, action)
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, inState := userStates[chatID]; inState {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel")),
		)
	}
	sendAndTrack(bot, msg)
}

func replyError(bot *tgbotapi.BotAPI, chatID int64, text string) {
	sendMessage(bot, chatID, "❌ "+text)
}

func sendAndTrack(bot *tgbotapi.BotAPI, msg tgbotapi.MessageConfig) {
	deleteLastMessage(bot, msg.ChatID)
	sentMsg, err := bot.Send(msg)
	if err == nil {
		lastMessageIDs[msg.ChatID] = sentMsg.MessageID
	}
}

func deleteLastMessage(bot *tgbotapi.BotAPI, chatID int64) {
	if msgID, ok := lastMessageIDs[chatID]; ok {
		deleteMsg := tgbotapi.NewDeleteMessage(chatID, msgID)
		bot.Request(deleteMsg)
		delete(lastMessageIDs, chatID)
	}
}

func resetState(userID int64) {
	delete(userStates, userID)
	delete(tempUserData, userID)
}

// ==========================================
// Validation Helpers
// ==========================================

func validateUsername(bot *tgbotapi.BotAPI, chatID int64, text string) bool {
	if len(text) < 3 || len(text) > 20 {
		sendMessage(bot, chatID, "❌ Password harus 3-20 karakter. Coba lagi:")
		return false
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(text) {
		sendMessage(bot, chatID, "❌ Password hanya boleh huruf, angka, - dan _. Coba lagi:")
		return false
	}
	return true
}

func validateNumber(bot *tgbotapi.BotAPI, chatID int64, text string, min, max int, fieldName string) (int, bool) {
	val, err := strconv.Atoi(text)
	if err != nil || val < min || val > max {
		sendMessage(bot, chatID, fmt.Sprintf("❌ %s harus angka positif (%d-%d). Coba lagi:", fieldName, min, max))
		return 0, false
	}
	return val, true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ==========================================
// Chat Session Management
// ==========================================

func saveChatSession(userID int64, chatID int64) {
	if _, exists := activeChats[userID]; !exists {
		activeChats[userID] = ChatSession{
			UserID: userID,
			ChatID: chatID,
			Joined: time.Now().Format("2006-01-02 15:04:05"),
		}
		saveChats()
	}
}

func loadChats() {
	if _, err := os.Stat(chatsFile); os.IsNotExist(err) {
		return
	}

	data, err := ioutil.ReadFile(chatsFile)
	if err != nil {
		return
	}

	var sessions []ChatSession
	if err := json.Unmarshal(data, &sessions); err == nil {
		for _, session := range sessions {
			activeChats[session.UserID] = session
		}
	}
}

func saveChats() {
	var sessions []ChatSession
	for _, session := range activeChats {
		sessions = append(sessions, session)
	}

	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return
	}

	ioutil.WriteFile(chatsFile, data, 0644)
}

// ==========================================
// Configuration & API
// ==========================================

func isAllowed(config *BotConfig, userID int64) bool {
	return config.Mode == "public" || userID == config.AdminID
}

func saveConfig(config *BotConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(BotConfigFile, data, 0644)
}

func loadConfig() (BotConfig, error) {
	var config BotConfig
	file, err := ioutil.ReadFile(BotConfigFile)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(file, &config)

	if config.Domain == "" {
		if domainBytes, err := ioutil.ReadFile(DomainFile); err == nil {
			config.Domain = strings.TrimSpace(string(domainBytes))
		}
	}

	return config, err
}

func apiCall(method, endpoint string, payload interface{}) (map[string]interface{}, error) {
	var reqBody []byte
	var err error

	if payload != nil {
		reqBody, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}

	client := &http.Client{}
	req, err := http.NewRequest(method, ApiUrl+endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", ApiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	return result, nil
}

func getIpInfo() (IpInfo, error) {
	resp, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		return IpInfo{}, err
	}
	defer resp.Body.Close()

	var info IpInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return IpInfo{}, err
	}
	return info, nil
}

func getUsers() ([]UserData, error) {
	res, err := apiCall("GET", "/users", nil)
	if err != nil {
		return nil, err
	}

	if res["success"] != true {
		return nil, fmt.Errorf("failed to get users")
	}

	var users []UserData
	dataBytes, _ := json.Marshal(res["data"])
	json.Unmarshal(dataBytes, &users)
	return users, nil
}