package uploader

import (
        "bytes"
        "encoding/base64"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "os"
        "path/filepath"
        "time"
)

const githubAPIBase = "https://api.github.com"

type GitHubUploader struct {
        token    string
        owner    string
        repo     string
        branch   string
        previewPath string
        client   *http.Client
}

type githubFileResponse struct {
        Content struct {
                SHA  string `json:"sha"`
                Path string `json:"path"`
        } `json:"content"`
}

type githubErrorResponse struct {
        Message string `json:"message"`
}

func NewGitHubUploader(token, repo, branch, previewPath string) *GitHubUploader {
        if branch == "" {
                branch = "main"
        }
        if previewPath == "" {
                previewPath = "previews"
        }
        owner, repoName := parseRepoString(repo)
        return &GitHubUploader{
                token:       token,
                owner:       owner,
                repo:        repoName,
                branch:      branch,
                previewPath: previewPath,
                client: newNoProxyClient(60 * time.Second),
        }
}

func parseRepoString(repo string) (owner, name string) {
        if idx := len(repo); idx > 0 {
                for i := 0; i < len(repo); i++ {
                        if repo[i] == '/' {
                                return repo[:i], repo[i+1:]
                        }
                }
        }
        return repo, "previews"
}

func (g *GitHubUploader) Upload(filePath string) (string, error) {
        if g.token == "" || g.owner == "" || g.repo == "" {
                return "", fmt.Errorf("github: token, owner, or repo not configured")
        }

        file, err := os.Open(filePath)
        if err != nil {
                return "", fmt.Errorf("github: open file: %w", err)
        }
        defer file.Close()

        data, err := io.ReadAll(file)
        if err != nil {
                return "", fmt.Errorf("github: read file: %w", err)
        }

        content := base64.StdEncoding.EncodeToString(data)
        filename := filepath.Base(filePath)
        path := g.previewPath + "/" + filename

        message := fmt.Sprintf("Add preview image: %s", filename)

        url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", githubAPIBase, g.owner, g.repo, path)

        body := map[string]string{
                "message": message,
                "content": content,
                "branch":  g.branch,
        }

        bodyBytes, err := json.Marshal(body)
        if err != nil {
                return "", fmt.Errorf("github: marshal body: %w", err)
        }

        req, err := http.NewRequest("PUT", url, bytes.NewBuffer(bodyBytes))
        if err != nil {
                return "", fmt.Errorf("github: create request: %w", err)
        }
        req.Header.Set("Authorization", "Bearer "+g.token)
        req.Header.Set("Accept", "application/vnd.github.v3+json")
        req.Header.Set("Content-Type", "application/json")

        resp, err := g.client.Do(req)
        if err != nil {
                return "", fmt.Errorf("github: send request: %w", err)
        }
        defer resp.Body.Close()

        respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024)) // 10MB limit
        if err != nil {
                return "", fmt.Errorf("github: read response: %w", err)
        }

        if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
                var errResp githubErrorResponse
                if json.Unmarshal(respBody, &errResp) == nil && errResp.Message != "" {
                        return "", fmt.Errorf("github: status %d: %s", resp.StatusCode, errResp.Message)
                }
                return "", fmt.Errorf("github: status %d: %s", resp.StatusCode, string(respBody))
        }

        var result githubFileResponse
        if err := json.Unmarshal(respBody, &result); err != nil {
                return "", fmt.Errorf("github: decode response: %w", err)
        }

        rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", g.owner, g.repo, g.branch, path)
        return rawURL, nil
}
