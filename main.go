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
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/net/html"
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

	log.Println("Go Worker Daemon Started...")

	for {
		processTracker(collection, telegramToken, chatID)
		processNews(collection, telegramToken, chatID)
		
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

func htmlToRichText(htmlText string) interface{} {
	doc, err := html.Parse(strings.NewReader("<div>" + htmlText + "</div>"))
	if err != nil {
		return htmlText
	}

	var root *html.Node

	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "div" {
			root = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
			if root != nil {
				return
			}
		}
	}

	find(doc)

	if root == nil {
		return htmlText
	}

	rich := convertChildren(root)

	if len(rich) == 1 {
		return rich[0]
	}

	return rich
}

func convertChildren(node *html.Node) []interface{} {
	var out []interface{}

	for c := node.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, convertNode(c)...)
	}

	return out
}


func convertNode(node *html.Node) []interface{} {

	switch node.Type {

	case html.TextNode:

		if node.Data == "" {
			return nil
		}

		return []interface{}{node.Data}

	case html.ElementNode:

		switch node.Data {

		case "strong", "b":

			return []interface{}{
				map[string]interface{}{
					"type": "bold",
					"text": convertChildren(node),
				},
			}

		case "em", "i":

			return []interface{}{
				map[string]interface{}{
					"type": "italic",
					"text": convertChildren(node),
				},
			}

		case "u":

			return []interface{}{
				map[string]interface{}{
					"type": "underline",
					"text": convertChildren(node),
				},
			}

		case "s", "strike":

			return []interface{}{
				map[string]interface{}{
					"type": "strikethrough",
					"text": convertChildren(node),
				},
			}

		case "code":

			return []interface{}{
				map[string]interface{}{
					"type": "code",
					"text": convertChildren(node),
				},
			}

		case "a":

			var href string

			for _, attr := range node.Attr {
				if attr.Key == "href" {
					href = attr.Val
					break
				}
			}

			return []interface{}{
				map[string]interface{}{
					"type": "url",
					"text": convertChildren(node),
					"url":  href,
				},
			}

		case "br":

			return []interface{}{"\n"}

		default:

			return convertChildren(node)
		}
	}

	return nil
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

func processNews(collection *mongo.Collection, token, chatID string) {
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

		if isPublished(collection, kvKey) {
			continue
		}

		fullResp, err := httpClient.Get(
			fmt.Sprintf("https://api.kresswell.me/kenyans/article/%s", slug),
		)
		if err != nil || fullResp.StatusCode != http.StatusOK {
			if fullResp != nil {
				fullResp.Body.Close()
			}
			continue
		}

		var fullArticle FullArticle
		if err := json.NewDecoder(fullResp.Body).Decode(&fullArticle); err != nil {
			fullResp.Body.Close()
			continue
		}
		fullResp.Body.Close()

		var blocks []map[string]interface{}

		// Heading
		blocks = append(blocks, map[string]interface{}{
			"type": "heading",
			"size": 1,
			"text": htmlToRichText(article.Headline),
		})

		// Images
		seen := make(map[string]bool)
		var photoBlocks []map[string]interface{}

		addPhoto := func(url string) {
			if url == "" || seen[url] {
				return
			}

			seen[url] = true

			photoBlocks = append(photoBlocks, map[string]interface{}{
				"type": "photo",
				"photo": map[string]interface{}{
					"url": url,
				},
			})
		}

		addPhoto(article.Thumbnail)

		for _, img := range fullArticle.Images {
			addPhoto(img)
		}

		if len(photoBlocks) > 0 {
			// blocks = append(blocks, map[string]interface{}{
			// 	"type":   "slideshow",
			// 	"blocks": photoBlocks,
			// })
		}

		// Paragraphs (RichText is HTML)
		paragraphs := strings.Split(fullArticle.Content, "\n\n")
		
		for _, para := range paragraphs {
		
			para = strings.TrimSpace(para)
		
			if para == "" {
				continue
			}
		
			blocks = append(blocks, map[string]interface{}{
				"type": "paragraph",
				"text": htmlToRichText(para),
			})
		}

		// Footer
		blocks = append(blocks, map[string]interface{}{
			"type": "footer",
			"text": []interface{}{
				"📰 Originally published on ",
				map[string]interface{}{
					"type": "url",
					"text": "Kenyans.co.ke",
					"url": fmt.Sprintf(
						"https://www.kenyans.co.ke/news/%s",
						slug,
					),
				},
			},
		})

		reqBody := map[string]interface{}{
			"chat_id": chatID,
			"rich_message": map[string]interface{}{
				"blocks": blocks,
			},
		}

		pretty, _ := json.MarshalIndent(reqBody, "", "  ")
		log.Println(string(pretty))

		tgURL := fmt.Sprintf(
			"https://api.telegram.org/bot%s/sendRichMessage",
			token,
		)

		if sendJSON(tgURL, reqBody) {
			markPublished(collection, kvKey)
			time.Sleep(3100 * time.Millisecond)
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
