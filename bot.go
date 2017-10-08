// -*- Go -*-
/* ------------------------------------------------ */
/* Golang source                                    */
/* Author: Alexei Panov <me@elemc.name>				*/
/* ------------------------------------------------ */

package main

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-pg/pg"
	log "github.com/sirupsen/logrus"
	"gopkg.in/telegram-bot-api.v4"
)

// PhotoCache is a struct for store thread-safe caches
type PhotoCache struct {
	cache map[int]string
	mutex sync.RWMutex
}

var (
	bot        *tgbotapi.BotAPI
	photoCache PhotoCache
	filesCache FilesCacheMemory

	// ErrorUserNotFound generic error for user is not found
	ErrorUserNotFound = fmt.Errorf("user not found")
)

func botServe() (err error) {
	var (
		updates <-chan tgbotapi.Update
	)
	defer wg.Done()

	photoCache.cache = make(map[int]string)
	filesCache.cache = make(map[string]string)

	if bot, err = tgbotapi.NewBotAPI(options.APIKey); err != nil {
		return
	}
	bot.Debug = options.Debug
	log.Debug("Telegram bot initialized sucessful")

	//go updatePhotoCache()
	//go filesCache.Update()

	updateOptions := tgbotapi.NewUpdate(0)
	updateOptions.Timeout = 60

	if updates, err = bot.GetUpdatesChan(updateOptions); err != nil {
		return
	}

	for update := range updates {
		if update.Message == nil {
			continue
		}
		// go func() {
		// 	if err = saveMessage(update.Message); err != nil {
		// 		log.Errorf("Unable to save message: %s", err)
		// 	}
		// }()

		// Insult
		//go insultMessage(update.Message)

		// command handler
		if update.Message.Command() != "" {
			go commandsMainHandler(update.Message)
		}
	}
	return
}

func saveMessage(msg *tgbotapi.Message) (err error) {
	// Files
	if msg.Audio != nil {
		go getFile(msg.Audio.FileID)
	}
	if msg.Document != nil {
		go getFile(msg.Document.FileID)
	}
	if msg.Photo != nil {
		for _, f := range *msg.Photo {
			go getFile(f.FileID)
		}
	}
	if msg.Sticker != nil {
		go getFile(msg.Sticker.FileID)
	}
	if msg.Video != nil {
		go getFile(msg.Video.FileID)
	}
	if msg.Voice != nil {
		go getFile(msg.Voice.FileID)
	}
	if msg.From != nil {
		go getUserPhoto(msg.From)
	}

	//return dbSaveMessage(msg)
	return nil
}

func getFile(fileID string) {
	var (
		err      error
		filename string
		f        tgbotapi.File
	)

	fc := tgbotapi.FileConfig{}
	fc.FileID = fileID
	if f, err = bot.GetFile(fc); err != nil {
		log.Errorf("Unable to get file FileID [%s]: %s", fileID, err)
		return
	}

	if filename, err = getFileName(fileID); err != nil {
		log.Errorf("Unable to get file name for file ID %s: %s", fileID, err)
		return
	}

	var stat os.FileInfo
	if stat, err = os.Stat(filename); err == nil {
		if stat.Size() == int64(f.FileSize) {
			log.Debugf("File %s found. Skip it.", filename)
			return
		}
	}

	// check directory
	path := filepath.Dir(filename)
	if err = os.MkdirAll(path, 0755); err != nil {
		log.Errorf("Unable to make directories for FileID [%s]: %s", fileID, err)
		return
	}

	if err = downloadImage(f.Link(options.APIKey), filename); err != nil {
		log.Errorf("Unable to download file for FileID [%s]: %s", fileID, err)
		return
	}
	if err = dbSaveFileToCahce(fileID, f.FilePath); err != nil {
		log.Errorf("Unabel to save file to cache ID %s file name %s: %s", fileID, filename, err)
		return
	}
	log.Debugf("File downloaded for FileID [%s] in %s", fileID, filename)
}

func getShortFileName(fileID string) (filename string) {
	var (
		fcache FileCache
		f      tgbotapi.File
		err    error
	)

	if fn := filesCache.Get(fileID); fn != "" {
		return fn
	}

	if fcache, err = getFileFromCache(fileID); err != nil && err != pg.ErrNoRows {
		log.Errorf("Unable to get file from cache file ID %s: %s", fileID, err)
	} else if err == nil {
		log.Debugf("File with ID %s found in cache: %s", fileID, fcache.FileName)
		return fcache.FileName
	}

	config := tgbotapi.FileConfig{FileID: fileID}
	if f, err = bot.GetFile(config); err != nil {
		return
	}
	filename = f.FilePath
	if err = dbSaveFileToCahce(fileID, filename); err != nil {
		log.Errorf("Unable to save file cache for ID=%s and file name %s: %s", fileID, filename, err)
	}

	return
}

func getFileName(fileID string) (filename string, err error) {
	if filename = getShortFileName(fileID); filename == "" {
		err = fmt.Errorf("unable to get file name for file ID %s", fileID)
		return
	}
	filename = filepath.Join(options.StaticDirPath, filename)
	return
}

func downloadImage(url string, filename string) (err error) {
	resp, err := http.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	file, err := os.Create(filename)
	if err != nil {
		return
	}
	defer file.Close()
	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return
	}
	return
}

func getUserPhotoFilename(user *tgbotapi.User) (filename string, err error) {
	photoCache.mutex.RLock()
	if fn, ok := photoCache.cache[user.ID]; ok {
		photoCache.mutex.RUnlock()
		if fn == "" {
			return
		}
		return filepath.Base(fn), nil
	}
	photoCache.mutex.RUnlock()

	if err = getUserPhoto(user); err != nil {
		return
	}

	photoCache.mutex.RLock()
	filename = photoCache.cache[user.ID]
	photoCache.mutex.RUnlock()

	if filename == "" {
		return
	}
	filename = filepath.Base(filename)

	return
}

func getUserPhoto(user *tgbotapi.User) (err error) {
	var (
		photos tgbotapi.UserProfilePhotos
		link   string
	)

	config := tgbotapi.NewUserProfilePhotos(user.ID)
	if photos, err = bot.GetUserProfilePhotos(config); err != nil {
		err = fmt.Errorf("Unable to get user profile photos for user with ID %d: %s", user.ID, err)
		return
	}
	if photos.TotalCount == 0 {
		return
	}

	if link, err = bot.GetFileDirectURL(photos.Photos[0][0].FileID); err != nil {
		err = fmt.Errorf("unable to get file direct URL for file ID %s", photos.Photos[0][0].FileID)
		return
	}

	fullFileName := filepath.Join(options.StaticDirPath, fmt.Sprintf("%d.jpg", user.ID))
	if err = downloadImage(link, fullFileName); err != nil {
		err = fmt.Errorf("Unable to download file %s to %s: %s", link, fullFileName, err)
		return
	}
	photoCache.mutex.Lock()
	photoCache.cache[user.ID] = fullFileName
	photoCache.mutex.Unlock()
	return
}

func updatePhotoCache() {
	log.Debugf("Start update photo cache...")
	var (
		users []tgbotapi.User
		err   error
	)
	if users, err = getUsers(); err != nil {
		log.Errorf("Unable to update photo cache: %s", err)
		return
	}

	for _, user := range users {
		go func(user tgbotapi.User) {
			if err = getUserPhoto(&user); err != nil {
				log.Errorf("Unable to get user photo: %s", err)
				return
			}
		}(user)
	}
	log.Debugf("Finish update photo cache.")
}

func sendMessage(chatID int64, text string, replyID int) {
	var (
		omsg tgbotapi.Message
		err  error
	)

	blockSize := 4096
	if len(text) > blockSize {
		log.Debugf("Message to big, size %d, must be cut", len(text))
		sendMessage(chatID, "* Сообщение слишком большое. Текст будет обрезан! *", replyID)
		suffix := ""
		if strings.HasPrefix(text, "```") {
			blockSize = 4088
			suffix = "```"
		}
		text = text[:blockSize]
		text += suffix
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if replyID != 0 {
		msg.ReplyToMessageID = replyID
	}
	if omsg, err = bot.Send(msg); err != nil {
		// oops, try to send as plain text
		log.Warnf("oops, unable to send markdown message [%s]: %s. Try to send as plain text.", text, err)
		msg.ParseMode = ""
		if omsg, err = bot.Send(msg); err != nil {
			log.Errorf("Unable to send message to %d with text [%s] and reply [%d]: %s", chatID, text, replyID, err)
			return
		}
	}

	if err = saveMessage(&omsg); err != nil {
		log.Errorf("Unable to save outgoing message: %s", err)
	}
}

func isUserAdmin(chat *tgbotapi.Chat, user *tgbotapi.User) bool {
	if chat == nil {
		return false
	}
	var err error

	config := tgbotapi.ChatConfig{
		ChatID: chat.ID,
	}

	var admins []tgbotapi.ChatMember
	if admins, err = bot.GetChatAdministrators(config); err != nil {
		log.Errorf("Unable to get chat administrators: %s", err)
		return false
	}

	for _, admin := range admins {
		if admin.User.ID == user.ID {
			return true
		}
	}
	return false
}

func sendMessageToAdmins(msg *tgbotapi.Message) {
	var err error
	config := tgbotapi.ChatConfig{
		ChatID: msg.Chat.ID,
	}

	var admins []tgbotapi.ChatMember
	if admins, err = bot.GetChatAdministrators(config); err != nil {
		log.Errorf("Unable to get chat administrators: %s", err)
		return
	}

	link := fmt.Sprintf("Новая жалоба на спам: https://t.me/%s/%d", msg.Chat.UserName, msg.ReplyToMessage.MessageID)

	for _, admin := range admins {
		go sendMessage(int64(admin.User.ID), link, 0)
	}
}

func isMeAdmin(chat *tgbotapi.Chat) bool {
	var (
		me  tgbotapi.User
		err error
	)

	if me, err = bot.GetMe(); err != nil {
		log.Errorf("Unable to get me: %s", err)
		return false
	}
	return isUserAdmin(chat, &me)
}

func sendMessageToAllChats(text string) {
	if text == "" {
		log.Warn("Unable to send empty message to all chats!")
		return
	}

	var (
		chats []tgbotapi.Chat
		err   error
	)
	if chats, err = getChats(); err != nil {
		log.Errorf("Unable to get all chats in sending message [%s] to all chats: %s", text, err)
		return
	}

	for _, chat := range chats {
		if !chat.IsGroup() && !chat.IsSuperGroup() && !chat.IsChannel() { // skip channels, private and other chats
			continue
		}

		go sendMessage(chat.ID, text, 0)
	}
}

func insultMessage(msg *tgbotapi.Message) {
	var (
		targets []string
		words   []string
		err     error
	)
	if targets, err = dbInsultGetWordsOrTargets(false); err != nil {
		log.Errorf("Unable to get insult targets: %s", err)
		return
	}
	if words, err = dbInsultGetWordsOrTargets(true); err != nil {
		log.Errorf("Unable to get insult words: %s", err)
		return
	}

	for _, target := range targets {
		if strings.Contains(strings.ToLower(msg.Text), strings.ToLower(target)) {
			r := rand.New(rand.NewSource(time.Now().UnixNano()))
			if r.Intn(4) == 0 {
				log.Debugf("Found %s in message. Skip it randomly.", target)
				break
			}

			// break if target word in URL
			re := regexp.MustCompile(`(http|ftp|https):\/\/([\w\-_]+(?:(?:\.[\w\-_]+)+))([\w\-\.,@?^=%&amp;:/~\+#]*[\w\-\@?^=%&amp;/~\+#])?`)
			for _, url := range re.FindAllString(msg.Text, -1) {
				if strings.Contains(strings.ToLower(url), strings.ToLower(target)) {
					log.Debugf("Target word \"%s\" in URL [%s]", target, url)
					return
				}
			}

			random := r.Int63n(int64(len(words)))
			sendMessage(msg.Chat.ID, fmt.Sprintf("%s - %s", target, words[random]), msg.MessageID)
			log.Debugf("Found %s in message. Answer %s - %s.", target, target, words[random])
			break
		}
	}
}
