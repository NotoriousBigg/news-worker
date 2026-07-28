package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

var httpClient = &http.Client{Timeout: 30 * time.Second}

func main() {
	telegramToken := os.Getenv("TELEGRAM_TOKEN")
	chatID := os.Getenv("CHAT_ID")
	mongoURI := os.Getenv("MONGO_URI")

	if telegramToken == "" || chatID == "" || mongoURI == "" {
		log.Fatal("Missing required environment variables: TELEGRAM_TOKEN, CHAT_ID, MONGO_URI")
	}

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

	// Read backwards from end of slice to preserve chronological stream order
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

				if sendJSON(tgURL, payload) {
					markPublished(collection, newsID)
					time.Sleep(3100 * time.Millisecond) // Bound rate metrics cleanly
				}
			}
		}
	}
}

func processNews(collection *mongo.Collection, token, chatID string, linkRegex *regexp.Regexp) {
	resp, err := httpClient.Get("https://api.kresswell.me/kenyans/news")
	if err != nil {
		log.Printf("Failed to fetch news index: %v", err)
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
				if fullResp != nil {
					fullResp.Body.Close()
				}
				continue
			}

			var fullArticle FullArticle
			json.NewDecoder(fullResp.Body).Decode(&fullArticle)
			fullResp.Body.Close()

			var blocks []map[string]interface{}

			// 1. Fixed Heading Block Schema
			blocks = append(blocks, map[string]interface{}{
				"type":  "heading",
				"level": 1, // Fix: Changed key name from 'size' to 'level'
				"text": map[string]interface{}{
					"text": article.Headline, // Fix: Flat direct text string assignment
				},
			})

			// 2. Swappable Slideshow Grid Layout block
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
				var photos []map[string]interface{}
				for idx, img := range uniqueImages {
					if idx >= 10 {
						break
					}
					photos = append(photos, map[string]interface{}{
						"url":    img,
						"width":  1024,
						"height": 576,
					})
				}
				blocks = append(blocks, map[string]interface{}{
					"type":   "slideshow",
					"photos": photos,
				})
			}

			// 3. Fixed Paragraph Segment Elements Mapping Loop
			paragraphs := strings.Split(fullArticle.Content, "\n\n")
			for _, para := range paragraphs {
				para = strings.TrimSpace(para)
				if para == "" {
					continue
				}

				matches := linkRegex.FindAllStringSubmatchIndex(para, -1)
				var segments []map[string]interface{}
				lastIndex := 0

				for _, match := range matches {
					if match[0] > lastIndex {
						segments = append(segments, map[string]interface{}{
							"text": para[lastIndex:match[0]],
						})
					}
					segments = append(segments, map[string]interface{}{
						"type": "url",
						"text": para[match[4]:match[5]], // Direct title string segment
						"url":  para[match[2]:match[3]],
					})
					lastIndex = match[1]
				}

				if lastIndex < len(para) {
					segments = append(segments, map[string]interface{}{
						"text": para[lastIndex:],
					})
				}

				// Build structural block mapping configurations using corrected conditional AST syntax
				paragraphBlock := map[string]interface{}{"type": "paragraph"}
				if len(segments) > 0 {
					paragraphBlock["text"] = map[string]interface{}{
						"segments": segments,
					}
				} else {
					paragraphBlock["text"] = map[string]interface{}{
						"text": para,
					}
				}
				blocks = append(blocks, paragraphBlock)
			}

			// 4. Clean Action Footer Block Setup
			blocks = append(blocks, map[string]interface{}{
				"type": "paragraph",
				"text": map[string]interface{}{
					"segments": []map[string]interface{}{
						{"text": "📰 Originally published on "},
						{
							"type": "url",
							"text": "Kenyans.co.ke",
							"url":  fmt.Sprintf("https://www.kenyans.co.ke/news/%s", slug),
						},
					},
				},
			})

			reqBody := map[string]interface{}{
				"chat_id": chatID,
				"rich_message": map[string]interface{}{
					"blocks": blocks,
				},
			}

			tgURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendRichMessage", token)
			
			// Fix: Verify transmission state before writing into MongoDB collection
			if sendJSON(tgURL, reqBody) {
				markPublished(collection, kvKey)
				time.Sleep(3100 * time.Millisecond)
			}
		}
	}
}

// Utility wrapper updated to trace transmission state indicators safely
func sendJSON(url string, payload interface{}) bool {
	body, _ := json.Marshal(payload)
	resp, err := httpClient.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		log.Printf("Network transport failure encountered: %v", err)
		return false
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("Telegram Server Refused Message: Status %d, Details: %s", resp.StatusCode, string(respBody))
		return false
	}
	return true
}
