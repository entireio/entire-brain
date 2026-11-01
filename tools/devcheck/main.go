// Command devcheck supplies small Go helpers for local release and UI checks.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: devcheck uuid|json-contains|distill-written|first-fact|remote-fact|commands")
	}
	switch args[0] {
	case "uuid":
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		id[6] = id[6]&15 | 64
		id[8] = id[8]&63 | 128
		_, err := fmt.Fprintf(output, "%x-%x-%x-%x-%x\n", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
		return err
	case "commands":
		if len(args) != 2 {
			return errors.New("commands requires a binary path")
		}
		return commands(args[1], nil, output)
	case "remote-fact":
		if len(args) != 5 {
			return errors.New("remote-fact requires API URL, repo ID, branch, and text")
		}
		return remoteFact(args[1], args[2], args[3], args[4])
	case "json-contains", "distill-written", "first-fact":
		var value any
		if err := json.NewDecoder(input).Decode(&value); err != nil {
			return err
		}
		if args[0] == "json-contains" {
			if len(args) != 2 || args[1] == "" {
				return errors.New("json-contains requires nonempty text")
			}
			if contains(value, args[1]) {
				return nil
			}
			return errors.New("JSON does not contain the requested text")
		}
		object, ok := value.(map[string]any)
		if !ok {
			return errors.New("expected a JSON object")
		}
		if args[0] == "distill-written" {
			for _, key := range []string{"facts_written", "memories_written", "facts"} {
				if n, exists := object[key]; exists {
					switch n := n.(type) {
					case float64:
						if n >= 1 {
							return nil
						}
					case []any:
						if len(n) > 0 {
							return nil
						}
					}
					break
				}
			}
			return errors.New("distill wrote no facts")
		}
		facts, _ := object["facts"].([]any)
		if len(facts) > 0 {
			fact, _ := facts[0].(map[string]any)
			for _, key := range []string{"text", "content"} {
				if text, ok := fact[key].(string); ok && text != "" {
					_, err := fmt.Fprintln(output, text)
					return err
				}
			}
		}
		return errors.New("recall returned no fact text")
	default:
		return fmt.Errorf("unknown helper %q", args[0])
	}
}

func contains(value any, needle string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, needle)
	case []any:
		for _, child := range v {
			if contains(child, needle) {
				return true
			}
		}
	case map[string]any:
		for _, child := range v {
			if contains(child, needle) {
				return true
			}
		}
	}
	return false
}

func commands(binary string, path []string, output io.Writer) error {
	if len(path) > 32 {
		return errors.New("command tree exceeds 32 levels")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := append([]string{"__complete"}, path...)
	data, err := exec.CommandContext(ctx, binary, append(args, "")...).Output()
	if err != nil {
		return fmt.Errorf("list commands: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, ":") {
			break
		}
		name, _, _ := strings.Cut(line, "\t")
		if name == "" || name == "help" || strings.HasPrefix(name, "-") {
			continue
		}
		child := append(append([]string{}, path...), name)
		if _, err := fmt.Fprintln(output, strings.Join(child, " ")); err != nil {
			return err
		}
		if err := commands(binary, child, output); err != nil {
			return err
		}
	}
	return nil
}

func remoteFact(api, repo, branch, needle string) error {
	token := os.Getenv("ENTIRE_TOKEN")
	if token == "" || needle == "" {
		return errors.New("remote-fact requires a token and nonempty text")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	page := ""
	for range 100 {
		query := url.Values{"pageSize": {"500"}}
		if page != "" {
			query.Set("pageToken", page)
		}
		request, err := http.NewRequest(http.MethodGet, strings.TrimRight(api, "/")+"/repos/"+url.PathEscape(repo)+"/brain/facts?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("User-Agent", "entire-release-smoke/1")
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		var body struct {
			Items         []struct{ Record struct{ Branch, Text string } } `json:"items"`
			NextPageToken string                                           `json:"nextPageToken"`
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return fmt.Errorf("facts request returned HTTP %d", response.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&body)
		response.Body.Close()
		if err != nil {
			return err
		}
		for _, item := range body.Items {
			if item.Record.Branch == branch && strings.Contains(item.Record.Text, needle) {
				return nil
			}
		}
		page = body.NextPageToken
		if page == "" {
			break
		}
	}
	return errors.New("fact not found in the hosted store")
}
