package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// --- API Response Structs ---

type TrackerItem struct {
	Time    string `json:"time"`
	Message string `json:"message"`
}

type TrackerDay struct {
	Date string        `json:"date"`
	News []TrackerItem `json:"news"`
}

type NewsArticle struct {
	Link      string `json:"link"`
	Headline  string `json:"headline"`
	Teaser    string `json:"teaser"`
	Thumbnail string `json:"thumbnail"`
}

type FullArticle struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Images  []string `json:"images"`
}

// --- Telegram Rich Message Structs ---

type TextSegment struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
}

type BlockText struct {
	Text     string        `json:"text,omitempty"`
	Segments []TextSegment `json:"segments,omitempty"`
}

type Photo struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type Block struct {
	Type   string     `json:"type"`
	Level  int        `json:"level,omitempty"`
	Text   *BlockText `json:"text,omitempty"`
	Photos []Photo    `json:"photos,omitempty"`
}

type SendRichMessageReq struct {
	ChatID      string `json:"chat_id"`
	RichMessage struct {
		Blocks []Block `json:"blocks"`
	} `json:"rich_message"`
}

// --- Global Client ---
var httpClient = &http.Client{Timeout: 30 * time.Second}

func main() {
	telegramToken := os.Getenv("TELEGRAM_TOKEN")
	chatID := os.Getenv("CHAT_ID")
	mongoURI := os.Getenv("MONGO_URI") // e.g., mongodb://mongodb:27017

	if telegramToken == "" || chatID == "" || mongoURI == "" {
		log.Fatal("Missing required environment variables: TELEGRAM_TOKEN, CHAT_ID, MONGO_URI")
	}

	// Connect to MongoDB
	clientOptions := options.Client().ApplyURI(mongoURI)
	dbClient, err := mongo.Connect(context.TODO(), clientOptions)
	if err != nil {
		log.Fatal(err)
	}
	defer dbClient.Disconnect(context.TODO())

	collection := dbClient.Database("kenyans_db").Collection("published_news")
	linkRegex := regexp.MustCompile(`<a href="([^"]+)">([^<]+)</a>`)

	log.Println("Go Worker Daemon Started...")

	for {
		processTracker(collection, telegramToken, chatID)
		processNews(collection, telegramToken, chatID, linkRegex)
		
		// Poll every 30 seconds
		time.Sleep(30 * time.Second)
	}
}

func isPublished(collection *mongo.Collection, id string) bool {
	var result bson.M
	err := collection.FindOne(context.TODO(), bson.M{"_id": id}).Decode(&result)
	return err == nil
}

func markPublished(collection *mongo.Collection, id string) {
	_, err := collection.InsertOne(context.TODO(), bson.M{"_id": id, "status": "published", "timestamp": time.Now()})
	if err != nil {
		log.Printf("Error inserting into DB: %v", err)
	}
}

func processTracker(collection *mongo.Collection, token, chatID string) {
	resp, err := httpClient.Get("https://api.kresswell.me/kenyans/tracker")
	if err != nil {
		log.Printf("Failed to fetch tracker: %v", err)
		return
	}
	defer resp.Body.Close()

	var data []TrackerDay
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("Failed to decode tracker JSON: %v", err)
		return
	}

	for i := len(data) - 1; i >= 0; i-- {
		day := data[i]
		for j := len(day.News) - 1; j >= 0; j-- {
			item := day.News[j]

			rawID := fmt.Sprintf("tracker-%s-%s", day.Date, item.Time)
			newsID := strings.ReplaceAll(strings.ReplaceAll(rawID, " ", ""), ",", "")

			if !isPublished(collection, newsID) {
				tgURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
				tgText := fmt.Sprintf("🕒 <b>%s - %s</b>\n\n%s", item.Time, day.Date, item.Message)

				payload := map[string]interface{}{
					"chat_id":              chatID,
					"text":                 tgText,
					"parse_mode":           "HTML",
					"link_preview_options": map[string]bool{"is_disabled": true},
				}

				sendJSON(tgURL, payload)
				markPublished(collection, newsID)
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
}

func processNews(collection *mongo.Collection, token, chatID string, linkRegex *regexp.Regexp) {
	resp, err := httpClient.Get("https://api.kresswell.me/kenyans/news")
	if err != nil {
		log.Printf("Failed to fetch news: %v", err)
		return
	}
	defer resp.Body.Close()

	var newsData []NewsArticle
	if err := json.NewDecoder(resp.Body).Decode(&newsData); err != nil {
		log.Printf("Failed to decode news JSON: %v", err)
		return
	}

	for i := len(newsData) - 1; i >= 0; i-- {
		article := newsData[i]
		slug := article.Link
		kvKey := "news-" + slug

		if !isPublished(collection, kvKey) {
			fullResp, err := httpClient.Get(fmt.Sprintf("https://api.kresswell.me/kenyans/article/%s", slug))
			if err != nil || fullResp.StatusCode != 200 {
				continue
			}

			var fullArticle FullArticle
			json.NewDecoder(fullResp.Body).Decode(&fullArticle)
			fullResp.Body.Close()

			var blocks []Block

			// Heading Block
			blocks = append(blocks, Block{
				Type:  "heading",
				Level: 1,
				Text:  &BlockText{Text: article.Headline},
			})

			// Slideshow Block (Deduplicate images)
			uniqueImages := []string{}
			seen := map[string]bool{}

			if article.Thumbnail != "" {
				uniqueImages = append(uniqueImages, article.Thumbnail)
				seen[article.Thumbnail] = true
			}

			for _, img := range fullArticle.Images {
				if img != "" && !seen[img] {
					uniqueImages = append(uniqueImages, img)
					seen[img] = true
				}
			}

			if len(uniqueImages) > 0 {
				photos := []Photo{}
				for i, img := range uniqueImages {
					if i >= 10 { // Limit to 10
						break
					}
					photos = append(photos, Photo{URL: img, Width: 1024, Height: 576})
				}
				blocks = append(blocks, Block{Type: "slideshow", Photos: photos})
			}

			// Paragraph Blocks
			paragraphs := strings.Split(fullArticle.Content, "\n\n")
			for _, para := range paragraphs {
				para = strings.TrimSpace(para)
				if para == "" {
					continue
				}

				matches := linkRegex.FindAllStringSubmatchIndex(para, -1)
				var segments []TextSegment
				lastIndex := 0

				for _, match := range matches {
					if match[0] > lastIndex {
						segments = append(segments, TextSegment{Text: para[lastIndex:match[0]]})
					}
					segments = append(segments, TextSegment{
						Type: "url",
						URL:  para[match[2]:match[3]],
						Text: para[match[4]:match[5]],
					})
					lastIndex = match[1]
				}

				if lastIndex < len(para) {
					segments = append(segments, TextSegment{Text: para[lastIndex:]})
				}

				if len(segments) > 0 {
					blocks = append(blocks, Block{Type: "paragraph", Text: &BlockText{Segments: segments}})
				} else {
					blocks = append(blocks, Block{Type: "paragraph", Text: &BlockText{Text: para}})
				}
			}

			// Footer notice Block
			blocks = append(blocks, Block{
				Type: "paragraph",
				Text: &BlockText{
					Segments: []TextSegment{
						{Text: "📰 Originally published on "},
						{Type: "url", Text: "Kenyans.co.ke", URL: fmt.Sprintf("https://www.kenyans.co.ke/news/%s", slug)},
					},
				},
			})

			// Send Rich Message
			reqBody := SendRichMessageReq{
				ChatID: chatID,
			}
			reqBody.RichMessage.Blocks = blocks

			tgURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendRichMessage", token)
			sendJSON(tgURL, reqBody)

			markPublished(collection, kvKey)
			time.Sleep(1 * time.Second) // Rate limit buffer
		}
	}
}

// Utility to send JSON payload
func sendJSON(url string, payload interface{}) {
	body, _ := json.Marshal(payload)
	resp, err := httpClient.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		log.Printf("Request error: %v", err)
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		// Read the exact error message from Telegram's response
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("Telegram API Error: Status %d, Details: %s", resp.StatusCode, string(respBody))
	}
}
