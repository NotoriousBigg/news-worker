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
	"regexp"

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

// RichTextEntity represents a Telegram MessageEntity
type RichTextEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	URL    string `json:"url,omitempty"`
}

// RichTextResult holds plain text and its entities
type RichTextResult struct {
	Text     string           `json:"text"`
	Entities []RichTextEntity `json:"entities"`
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


var unsupportedTagRegex = regexp.MustCompile(`</?(span|div|figure|figcaption|p|h[1-6]|br)\b[^>]*>`)

func stripUnsupportedHTML(input string) string {
	return unsupportedTagRegex.ReplaceAllString(input, "")
}
// htmlToRichText converts HTML to plain text with Telegram MessageEntity objects
// Uses byte offsets (NOT rune offsets) for entities
func htmlToRichText(htmlText string) RichTextResult {
	doc, err := html.Parse(strings.NewReader("<div>" + htmlText + "</div>"))
	if err != nil {
		return RichTextResult{Text: htmlText}
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
		return RichTextResult{Text: htmlText}
	}

	var textBuilder strings.Builder
	var entities []RichTextEntity

	extractTextAndEntities(root, &textBuilder, &entities, nil)

	if entities == nil {
        entities = []RichTextEntity{}
    }
    
    return RichTextResult{
        Text:     textBuilder.String(),
        Entities: entities,
    }
}

// entityStack tracks active formatting entities
type entityStack struct {
	Type   string
	Offset int
	URL    string
	Next   *entityStack
}

func pushStack(stack *entityStack, entityType string, url string) *entityStack {
	return &entityStack{
		Type:   entityType,
		Offset: -1,
		URL:    url,
		Next:   stack,
	}
}

func extractTextAndEntities(node *html.Node, builder *strings.Builder, entities *[]RichTextEntity, stack *entityStack) {
	switch node.Type {
	case html.TextNode:
		if node.Data == "" {
			return
		}

		currentOffset := builder.Len()
		s := stack
		for s != nil {
			if s.Offset == -1 {
				s.Offset = currentOffset
			}
			s = s.Next
		}

		builder.WriteString(node.Data)

	case html.ElementNode:
		switch node.Data {
		case "br":
			builder.WriteString("\n")
			return
		case "span", "div", "figure", "figcaption", "p", "h1", "h2", "h3", "h4", "h5", "h6":
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				extractTextAndEntities(c, builder, entities, stack)
			}
			return
		case "strong", "b":
			newStack := pushStack(stack, "bold", "")
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				extractTextAndEntities(c, builder, entities, newStack)
			}
			closeEntity(builder.Len(), newStack, entities)
			return
		case "em", "i":
			newStack := pushStack(stack, "italic", "")
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				extractTextAndEntities(c, builder, entities, newStack)
			}
			closeEntity(builder.Len(), newStack, entities)
			return
		case "u", "ins":
			newStack := pushStack(stack, "underline", "")
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				extractTextAndEntities(c, builder, entities, newStack)
			}
			closeEntity(builder.Len(), newStack, entities)
			return
		case "s", "strike", "del":
			newStack := pushStack(stack, "strikethrough", "")
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				extractTextAndEntities(c, builder, entities, newStack)
			}
			closeEntity(builder.Len(), newStack, entities)
			return
		case "code":
			newStack := pushStack(stack, "code", "")
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				extractTextAndEntities(c, builder, entities, newStack)
			}
			closeEntity(builder.Len(), newStack, entities)
			return
		case "pre":
			newStack := pushStack(stack, "pre", "")
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				extractTextAndEntities(c, builder, entities, newStack)
			}
			closeEntity(builder.Len(), newStack, entities)
			return
		case "a":
			var href string
			for _, attr := range node.Attr {
				if attr.Key == "href" {
					href = attr.Val
					break
				}
			}
			newStack := pushStack(stack, "text_link", href)
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				extractTextAndEntities(c, builder, entities, newStack)
			}
			closeEntity(builder.Len(), newStack, entities)
			return
		default:
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				extractTextAndEntities(c, builder, entities, stack)
			}
			return
		}
	}
}

func closeEntity(endOffset int, stack *entityStack, entities *[]RichTextEntity) {
	if stack == nil || stack.Offset == -1 {
		return
	}
	length := endOffset - stack.Offset
	if length > 0 {
		entity := RichTextEntity{
			Type:   stack.Type,
			Offset: stack.Offset,
			Length: length,
		}
		if stack.Type == "text_link" {
			entity.URL = stack.URL
		}
		*entities = append(*entities, entity)
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
				
				cleanMessage := stripUnsupportedHTML(item.Message)
				
				tgText := fmt.Sprintf("🕒 <b>%s - %s</b>\n\n%s", item.Time, day.Date, cleanMessage)

				payload := map[string]interface{}{
					"chat_id":              chatID,
					"text":                 tgText,
					"parse_mode":           "HTML",
					"link_preview_options": map[string]bool{"is_disabled": true},
				}

				if sendJSON(tgURL, payload) {
					markPublished(collection, newsID)
					time.Sleep(3100 * time.Millisecond)
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
		richHeadline := htmlToRichText(article.Headline)
		blocks = append(blocks, map[string]interface{}{
			"type":     "heading",
			"size":     1,
			"text":     richHeadline.Text,
		})

		// Images
		seen := make(map[string]bool)

		addPhoto := func(url string) {
			if url == "" || seen[url] {
				return
			}
			seen[url] = true
			blocks = append(blocks, map[string]interface{}{
				"type": "photo",
				"photo": map[string]interface{}{
					"media": url,
				},
			})
		}

		addPhoto(article.Thumbnail)

		for _, img := range fullArticle.Images {
			addPhoto(img)
		}

		// Paragraphs
		paragraphs := strings.Split(fullArticle.Content, "\n\n")
		
		for _, para := range paragraphs {
			para = strings.TrimSpace(para)
			if para == "" {
				continue
			}
		
			richPara := htmlToRichText(para)
			blocks = append(blocks, map[string]interface{}{
				"type":     "paragraph",
				"text":     richPara.Text,
			})
		}

		// Footer
		footerText := "📰 Originally published on Kenyans.co.ke"
		// footerEntities := []RichTextEntity{
		// 	{
		// 		Type:   "text_link",
		// 		Offset: 29,
		// 		Length: 13,
		// 		URL:    fmt.Sprintf("https://www.kenyans.co.ke/news/%s", slug),
		// 	},
		// }

		blocks = append(blocks, map[string]interface{}{
			"type":     "footer",
			"text":     footerText,
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
