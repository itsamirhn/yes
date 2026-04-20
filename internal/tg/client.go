package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	Token   string
	BaseURL string // e.g. "https://api.telegram.org/bot"
	HTTP    *http.Client
}

func NewClient(token, baseURL string) *Client {
	return &Client{
		Token:   token,
		BaseURL: baseURL,
		HTTP: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 32,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
			Timeout: 0, // managed per-request via context
		},
	}
}

func (c *Client) apiURL(method string) string {
	return c.BaseURL + c.Token + "/" + method
}

// fileBaseURL derives the file download base URL by inserting /file before /bot
func (c *Client) fileBaseURL() string {
	i := strings.LastIndex(c.BaseURL, "/bot")
	if i < 0 {
		return c.BaseURL
	}
	return c.BaseURL[:i] + "/file" + c.BaseURL[i:]
}

type apiResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Params *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
	Description string `json:"description"`
}

func (c *Client) doJSON(ctx context.Context, method string, body interface{}) (json.RawMessage, error) {
	for {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", c.apiURL(method), bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			log.Printf("HTTP error for %s: %v, retrying in 1s", method, err)
			time.Sleep(time.Second)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var apiResp apiResponse
		json.Unmarshal(raw, &apiResp)

		if resp.StatusCode == 429 && apiResp.Params != nil {
			wait := time.Duration(apiResp.Params.RetryAfter) * time.Second
			log.Printf("Flood control for %s, retrying in %v", method, wait)
			time.Sleep(wait)
			continue
		}
		if !apiResp.OK {
			return nil, fmt.Errorf("telegram %s: %s", method, apiResp.Description)
		}
		return apiResp.Result, nil
	}
}

func (c *Client) SendMessage(ctx context.Context, chatID string, text string) error {
	_, err := c.doJSON(ctx, "sendMessage", map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	})
	return err
}

type Update struct {
	UpdateID    int      `json:"update_id"`
	Message     *Message `json:"message"`
	ChannelPost *Message `json:"channel_post"`
}

type Message struct {
	Text     string    `json:"text"`
	Chat     Chat      `json:"chat"`
	Document *Document `json:"document"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
}

func (c *Client) GetUpdates(ctx context.Context, offset *int, limit int) ([]Update, error) {
	body := map[string]interface{}{"limit": limit, "timeout": 30}
	if offset != nil {
		body["offset"] = *offset
	}

	// Use a longer timeout for long polling
	longCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := c.doJSON(longCtx, "getUpdates", body)
	if err != nil {
		return nil, err
	}
	var updates []Update
	json.Unmarshal(result, &updates)
	return updates, nil
}

func (c *Client) SetWebhook(ctx context.Context, url string) error {
	_, err := c.doJSON(ctx, "setWebhook", map[string]interface{}{
		"url": url,
	})
	return err
}

func (c *Client) SendDocument(ctx context.Context, chatID string, data []byte, filename string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("chat_id", chatID)
	part, err := w.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	part.Write(data)
	w.Close()

	for {
		req, err := http.NewRequestWithContext(ctx, "POST", c.apiURL("sendDocument"), bytes.NewReader(buf.Bytes()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", w.FormDataContentType())

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("HTTP error for sendDocument: %v, retrying in 1s", err)
			time.Sleep(time.Second)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var apiResp apiResponse
		json.Unmarshal(raw, &apiResp)

		if resp.StatusCode == 429 && apiResp.Params != nil {
			wait := time.Duration(apiResp.Params.RetryAfter) * time.Second
			log.Printf("Flood control for sendDocument, retrying in %v", wait)
			time.Sleep(wait)
			continue
		}
		if !apiResp.OK {
			return fmt.Errorf("telegram sendDocument: %s", apiResp.Description)
		}
		return nil
	}
}

type fileResponse struct {
	FilePath string `json:"file_path"`
}

func (c *Client) GetFile(ctx context.Context, fileID string) (string, error) {
	result, err := c.doJSON(ctx, "getFile", map[string]interface{}{
		"file_id": fileID,
	})
	if err != nil {
		return "", err
	}
	var f fileResponse
	json.Unmarshal(result, &f)
	return f.FilePath, nil
}

func (c *Client) DownloadFile(ctx context.Context, filePath string) ([]byte, error) {
	url := c.fileBaseURL() + c.Token + "/" + filePath
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			log.Printf("Download error: %v, retrying in 1s", err)
			time.Sleep(time.Second)
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			log.Printf("Download returned %d, retrying in 1s", resp.StatusCode)
			time.Sleep(time.Second)
			continue
		}
		return data, nil
	}
}

func (c *Client) DownloadDocument(ctx context.Context, fileID string) ([]byte, error) {
	filePath, err := c.GetFile(ctx, fileID)
	if err != nil {
		return nil, err
	}
	return c.DownloadFile(ctx, filePath)
}
