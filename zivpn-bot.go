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
	Username string `json:"username"` // Tambah field username
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

	bot.Debug = true // Ubah ke true untuk debugging
	log.Printf("Bot berjalan sebagai %s (ID: %d)", bot.Self.UserName, bot.Self.ID)

	// Test koneksi ke API lokal
	if err := testLocalAPI(); err != nil {
		log.Printf("Peringatan: Gagal konek ke API lokal: %v", err)
	}

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
// Telegram Event Handlers - DIPERBAIKI
// ==========================================

func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	chatID := msg.Chat.ID
	userID := msg.From.ID
	username := msg.From.UserName

	log.Printf("Pesan dari %s (ID: %d): %s", username, userID, msg.Text)

	// Save chat session dengan username
	saveChatSession(userID, chatID, username)

	// Access Control
	if !isAllowed(config, userID) {
		log.Printf("Akses ditolak untuk user %d", userID)
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
		log.Printf("Command dari %d: %s", userID, msg.Command())
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
		case "status":
			checkStatus(bot, chatID, config)
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

	log.Printf("Callback dari %d: %s", userID, data)

	// Save chat session
	saveChatSession(userID, chatID, query.From.UserName)

	// Access Control
	if !isAllowed(config, userID) {
		if data != "toggle_mode" || userID != config.AdminID {
			bot.Request(tgbotapi.NewCallback(query.ID, "Akses Ditolak"))
			return
		}
	}

	// Handle callback types
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
	case data == "menu_message":
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
		log.Printf("Callback tidak dikenal: %s", data)
		bot.Request(tgbotapi.NewCallback(query.ID, "Aksi tidak dikenal"))
	}

	bot.Request(tgbotapi.NewCallback(query.ID, ""))
}

// ==========================================
// MESSAGING FEATURES - DIPERBAIKI
// ==========================================

func showMessageStats(bot *tgbotapi.BotAPI, chatID int64) {
	totalUsers := len(activeChats)
	adminCount := 0
	regularCount := 0

	for _, session := range activeChats {
		if session.Username == "" {
			regularCount++
		} else {
			adminCount++
		}
	}

	msg := fmt.Sprintf("📊 *Message Statistics*\n\n"+
		"• Total Active Chats: %d\n"+
		"• Admin Chats: %d\n"+
		"• Regular Users: %d\n"+
		"• Last Save: %s",
		totalUsers, adminCount, regularCount, lastSaveTime.Format("15:04:05"))

	reply := tgbotapi.NewMessage(chatID, msg)
	reply.ParseMode = "Markdown"
	bot.Send(reply)
}

func sendBroadcastMessage(bot *tgbotapi.BotAPI, chatID int64, message string, config *BotConfig) {
	if strings.ToLower(message) == "/cancel" || message == "cancel" {
		cancelOperation(bot, chatID, config.AdminID, config)
		return
	}

	totalSent := 0
	totalFailed := 0
	var failedUsers []string

	// Send to admin first as confirmation
	adminMsg := tgbotapi.NewMessage(chatID, 
		fmt.Sprintf("📤 *Mengirim Broadcast...*\n\nPesan: %s\n\n⏳ Mohon tunggu...", 
		truncateString(message, 100)))
	adminMsg.ParseMode = "Markdown"
	bot.Send(adminMsg)

	// Send to all active chats
	for userID, session := range activeChats {
		// Skip admin
		if userID == config.AdminID {
			continue
		}

		msg := tgbotapi.NewMessage(session.ChatID, 
			"📢 *BROADCAST MESSAGE FROM ADMIN*\n\n"+message)
		msg.ParseMode = "Markdown"
		
		// Add footer
		msg.Text += "\n\n_• Broadcast dari Admin •_"

		_, err := bot.Send(msg)
		if err != nil {
			log.Printf("Gagal mengirim ke user %d: %v", userID, err)
			totalFailed++
			failedUsers = append(failedUsers, session.Username)
		} else {
			totalSent++
		}

		// Delay to avoid rate limiting
		time.Sleep(50 * time.Millisecond)
	}

	// Save chats (jika ada perubahan)
	if time.Since(lastSaveTime) > time.Minute {
		saveChats()
	}

	// Send report to admin
	reportMsg := fmt.Sprintf("✅ *Broadcast Selesai!*\n\n📊 Statistik:\n"+
		"• Berhasil: %d user\n"+
		"• Gagal: %d user\n"+
		"• Total Active: %d user",
		totalSent, totalFailed, len(activeChats)-1)

	if totalFailed > 0 {
		reportMsg += fmt.Sprintf("\n\n❌ Gagal dikirim ke: %s", 
			strings.Join(failedUsers[:min(5, len(failedUsers))], ", "))
	}

	reply := tgbotapi.NewMessage(chatID, reportMsg)
	reply.ParseMode = "Markdown"
	bot.Send(reply)

	showMainMenu(bot, chatID, config)
}

// Fungsi helper untuk truncate string
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ==========================================
// CHAT SESSION MANAGEMENT - DIPERBAIKI
// ==========================================

func saveChatSession(userID int64, chatID int64, username string) {
	now := time.Now()
	
	// Update atau buat session baru
	activeChats[userID] = ChatSession{
		UserID:   userID,
		ChatID:   chatID,
		Username: username,
		Joined:   now.Format("2006-01-02 15:04:05"),
	}

	// Save ke file maksimal setiap 30 detik
	if time.Since(lastSaveTime) > 30*time.Second {
		saveChats()
		lastSaveTime = now
	}
}

func loadChats() {
	if _, err := os.Stat(chatsFile); os.IsNotExist(err) {
		log.Println("File chats.json belum ada, akan dibuat baru")
		return
	}

	data, err := ioutil.ReadFile(chatsFile)
	if err != nil {
		log.Printf("Gagal membaca file chats: %v", err)
		return
	}

	var sessions []ChatSession
	if err := json.Unmarshal(data, &sessions); err != nil {
		log.Printf("Gagal parse chats JSON: %v", err)
		return
	}

	for _, session := range sessions {
		activeChats[session.UserID] = session
	}
	
	log.Printf("Loaded %d chat sessions", len(sessions))
	lastSaveTime = time.Now()
}

func saveChats() {
	var sessions []ChatSession
	for _, session := range activeChats {
		sessions = append(sessions, session)
	}

	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		log.Printf("Gagal marshal chats: %v", err)
		return
	}

	if err := ioutil.WriteFile(chatsFile, data, 0644); err != nil {
		log.Printf("Gagal save chats: %v", err)
	}
}

// ==========================================
// UTILITY FUNCTIONS - DIPERBAIKI
// ==========================================

func testLocalAPI() error {
	resp, err := http.Get(ApiUrl + "/info")
	if err != nil {
		return fmt.Errorf("API tidak merespon: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("API returned status: %d", resp.StatusCode)
	}

	log.Println("Koneksi ke API lokal: OK")
	return nil
}

func checkStatus(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	// Test koneksi ke API lokal
	apiStatus := "✅"
	if err := testLocalAPI(); err != nil {
		apiStatus = "❌ " + err.Error()
	}

	// Test koneksi ke Telegram
	telegramStatus := "✅"
	if _, err := bot.GetMe(); err != nil {
		telegramStatus = "❌ " + err.Error()
	}

	msg := fmt.Sprintf("🔄 *System Status*\n\n"+
		"• Bot: %s (@%s)\n"+
		"• Local API: %s\n"+
		"• Telegram API: %s\n"+
		"• Active Chats: %d\n"+
		"• Mode: %s",
		bot.Self.FirstName, bot.Self.UserName,
		apiStatus, telegramStatus,
		len(activeChats), config.Mode)

	reply := tgbotapi.NewMessage(chatID, msg)
	reply.ParseMode = "Markdown"
	bot.Send(reply)
}

// ==========================================
// CONFIGURATION & API - DIPERBAIKI
// ==========================================

func getUsers() ([]UserData, error) {
	res, err := apiCall("GET", "/users", nil)
	if err != nil {
		log.Printf("Error API call: %v", err)
		return nil, err
	}

	if res["success"] != true {
		msg := "unknown error"
		if m, ok := res["message"].(string); ok {
			msg = m
		}
		return nil, fmt.Errorf("API error: %s", msg)
	}

	// Konversi data dengan error handling
	data, ok := res["data"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid data format from API")
	}

	var users []UserData
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal data: %v", err)
	}

	if err := json.Unmarshal(dataBytes, &users); err != nil {
		return nil, fmt.Errorf("failed to unmarshal users: %v", err)
	}

	return users, nil
}

func apiCall(method, endpoint string, payload interface{}) (map[string]interface{}, error) {
	// Pastikan API key sudah loaded
	if ApiKey == "" {
		if keyBytes, err := ioutil.ReadFile(ApiKeyFile); err == nil {
			ApiKey = strings.TrimSpace(string(keyBytes))
		}
	}

	var reqBody []byte
	var err error

	if payload != nil {
		reqBody, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(method, ApiUrl+endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", ApiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("JSON parse error: %v", err)
	}

	return result, nil
}

// ==========================================
// BAGIAN LAIN TETAP SAMA
// ==========================================

// [Semua fungsi lainnya tetap sama seperti sebelumnya...]
// Hanya bagian-bagian kritis yang saya perbaiki di atas