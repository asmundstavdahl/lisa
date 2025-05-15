package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

var db *sql.DB
var tpl *template.Template

const dbPath = "./lisa.db"

type Message struct {
	ID        int64     `json:"id"`
	Sender    string    `json:"sender"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

type LLMConfig struct {
	APIKey  string
	BaseURL string
}

// --- Structs for Function Calling --- START ---
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type Function struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
}

type FunctionParameters struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required,omitempty"`
}

type Property struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// --- Structs for Function Calling --- END ---

type LLMMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"` // For tool role, name of the function
}

type LLMRequest struct {
	Model    string       `json:"model"`
	Messages []LLMMessage `json:"messages"`
	Tools    []Tool       `json:"tools,omitempty"`
	Stream   bool         `json:"stream,omitempty"`
}

type LLMResponse struct {
	Choices []struct {
		Message      LLMMessage `json:"message"`
		FinishReason string     `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// Memory struct for database interaction
type Memory struct {
	ID             int64        `json:"id"`
	Content        string       `json:"content"`
	CreatedAt      time.Time    `json:"created_at"`
	LastAccessedAt sql.NullTime `json:"last_accessed_at"`
	Metadata       string       `json:"metadata"` // Store as JSON string
}

type CreateMemoryArgs struct {
	Content    string `json:"content"`
	Type       string `json:"type,omitempty"`
	Importance int    `json:"importance,omitempty"`
}

type RetrieveMemoriesArgs struct {
	Query string `json:"query"`
	Count int    `json:"count,omitempty"`
}

func init() {
	log.Println("DEBUG: init() function started.")
	var err error
	tpl = template.Must(template.ParseGlob("templates/*.html"))
	log.Println("DEBUG: Templates parsed.")

	log.Printf("DEBUG: Attempting to open database at %s...", dbPath)
	db, err = sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_journal_mode=WAL") // Added WAL mode for potentially better concurrency
	if err != nil {
		log.Fatalf("DEBUG: FATAL - Failed to open database: %v", err)
	}
	log.Println("DEBUG: Database opened successfully.")

	// Ping the database to ensure connection is live
	if err = db.Ping(); err != nil {
	    log.Fatalf("DEBUG: FATAL - Failed to ping database: %v", err)
	}
	log.Println("DEBUG: Database ping successful.")

	// Create tables if they don't exist
	createTablesSQL := `
	CREATE TABLE IF NOT EXISTS conversations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		sender TEXT NOT NULL,
		message TEXT NOT NULL,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT
	);
	CREATE TABLE IF NOT EXISTS memories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		content TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_accessed_at DATETIME,
		metadata TEXT -- Store as JSON: {"type": "preferanse", "importance": 3}
	);
	`
	log.Println("DEBUG: Attempting to execute createTablesSQL...")
	if _, err := db.Exec(createTablesSQL); err != nil {
		log.Fatalf("DEBUG: FATAL - Failed to create tables: %v. SQL:\n%s", err, createTablesSQL)
	}
	log.Println("DEBUG: Database tables checked/created successfully.")

	// Ensure default settings are present
	log.Println("DEBUG: Ensuring default settings are present...")
	defaultSettings := []struct {
		key   string
		value string
	}{
		{"llm_api_key", "YOUR_API_KEY_HERE"},
		{"llm_base_url", "YOUR_LLM_BASE_URL_HERE"},
		{"llm_model", "llama3-8b-8192"},
	}

	log.Println("DEBUG: Preparing statement for INSERT OR IGNORE INTO settings...")
	stmt, err := db.Prepare("INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)")
	if err != nil {
		log.Fatalf("DEBUG: FATAL - Failed to prepare settings insert statement: %v", err)
	}
	log.Println("DEBUG: Statement prepared successfully.")

	for i, setting := range defaultSettings {
		log.Printf("DEBUG: Attempting to insert/ignore setting %d: key=\"%s\", value=\"%s\"", i+1, setting.key, setting.value)
		if _, err := stmt.Exec(setting.key, setting.value); err != nil {
			log.Printf("DEBUG: ERROR - Failed to insert/ignore default setting %s: %v. Closing statement.", setting.key, err)
			stmt.Close()
			log.Fatalf("DEBUG: FATAL - Error was: %v", err) // Fatal after logging details
		}
		log.Printf("DEBUG: Successfully inserted/ignored setting %d: key=\"%s\"", i+1, setting.key)
	}
	log.Println("DEBUG: Closing prepared statement for settings insert.")
	stmt.Close()
	log.Println("DEBUG: Default settings check complete. Update API key/URL if using defaults.")
	log.Println("DEBUG: init() function finished.")
}


func getLLMConfig() (LLMConfig, error) {
	log.Println("DEBUG: getLLMConfig() called.")
	var config LLMConfig
	log.Println("DEBUG: Querying database for llm_api_key and llm_base_url...")
	rows, err := db.Query("SELECT key, value FROM settings WHERE key = 'llm_api_key' OR key = 'llm_base_url'")
	if err != nil {
		log.Printf("DEBUG: Error querying settings in getLLMConfig: %v", err)
		return config, fmt.Errorf("failed to query settings: %w", err)
	}
	defer rows.Close()

	log.Println("DEBUG: Iterating through settings rows...")
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			log.Printf("DEBUG: Error scanning setting row in getLLMConfig: %v", err)
			return config, fmt.Errorf("failed to scan setting row: %w", err)
		}
		log.Printf("DEBUG: Scanned setting: key='%s', value='%s'", key, value) // Log each scanned key-value pair
		if key == "llm_api_key" {
			config.APIKey = value
		} else if key == "llm_base_url" {
			config.BaseURL = value
		}
	}
	if err = rows.Err(); err != nil {
		log.Printf("DEBUG: Settings rows iteration error in getLLMConfig: %v", err)
		return config, fmt.Errorf("settings rows iteration error: %w", err)
	}

	log.Printf("DEBUG: After loop, config.APIKey: '%s'", config.APIKey)
	log.Printf("DEBUG: After loop, config.BaseURL: '%s'", config.BaseURL)

	if config.APIKey == "" || config.BaseURL == "" || config.APIKey == "YOUR_API_KEY_HERE" || config.BaseURL == "YOUR_LLM_BASE_URL_HERE" {
		log.Printf("DEBUG: LLM API key or Base URL not configured or using default. APIKey: '%s', BaseURL: '%s'", config.APIKey, config.BaseURL)
		return config, fmt.Errorf("LLM API key or Base URL not configured in database settings")
	}
	log.Println("DEBUG: getLLMConfig() successful.")
	return config, nil
}

func getLLMModel() (string, error) {
	log.Println("DEBUG: getLLMModel() called.")
	var model string
	log.Println("DEBUG: Querying database for llm_model...")
	err := db.QueryRow("SELECT value FROM settings WHERE key = 'llm_model'").Scan(&model)
	if err != nil {
		log.Printf("DEBUG: Error querying llm_model from settings in getLLMModel: %v", err)
		if err == sql.ErrNoRows {
			log.Println("DEBUG: LLM model not found in settings (sql.ErrNoRows), attempting to use default from init logic: llama3-8b-8192")
			// This path implies init() might not have run or completed as expected if the key is missing.
			// For robustness, we could try to re-fetch or simply return the default and log it.
			// However, if init() guarantees the key, this should be an exceptional case.
			return "llama3-8b-8192", fmt.Errorf("llm_model not found in settings (sql.ErrNoRows), init might have failed to insert default: %w", err)
		}
		return "", fmt.Errorf("failed to query llm_model from settings: %w", err)
	}

	log.Printf("DEBUG: Scanned llm_model: '%s'", model)

	if model == "" {
		log.Println("DEBUG: LLM model is empty in settings, defaulting to llama3-8b-8192")
		return "llama3-8b-8192", nil // Default model
	}
	log.Println("DEBUG: getLLMModel() successful.")
	return model, nil
}

func storeMessage(sender, messageText string) error {
	stmt, err := db.Prepare("INSERT INTO conversations(sender, message) VALUES(?, ?)")
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	_, err = stmt.Exec(sender, messageText)
	if err != nil {
		return fmt.Errorf("failed to execute statement: %w", err)
	}
	return nil
}

func getHistory() ([]Message, error) {
	rows, err := db.Query("SELECT id, sender, message, timestamp FROM conversations ORDER BY timestamp ASC LIMIT 20") 
	if err != nil {
		return nil, fmt.Errorf("failed to query history: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var tsStr string
		if err := rows.Scan(&msg.ID, &msg.Sender, &msg.Message, &tsStr); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		parsedTime, parseErr := time.Parse("2006-01-02 15:04:05", tsStr)
		if parseErr != nil {
			parsedTime, parseErr = time.Parse(time.RFC3339Nano, tsStr) 
			if parseErr != nil {
				log.Printf("Warning: could not parse timestamp string '%s': %v. Using current time.", tsStr, parseErr)
				msg.Timestamp = time.Now()
			} else {
				msg.Timestamp = parsedTime
			}
		} else {
			msg.Timestamp = parsedTime
		}
		messages = append(messages, msg)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return messages, nil
}

func getTools() []Tool {
	return []Tool{
		{
			Type: "function",
			Function: Function{
				Name:        "create_memory",
				Description: "Lagrer en informasjonsbit (et minne) til langtidshukommelsen. Brukes når brukeren oppgir et spesifikt faktum, en preferanse eller en instruksjon som bør huskes for fremtidige interaksjoner.",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]Property{
						"content":    {Type: "string", Description: "Den tekstlige informasjonen som skal lagres som et minne. Eksempel: Brukerens favorittfarge er blå."},
						"type":       {Type: "string", Description: "En kategori for minnet. Eksempler: preferanse, faktum, instruksjon, personlig_detalj. Standard: generelt"},
						"importance": {Type: "integer", Description: "En numerisk verdi som indikerer viktigheten av minnet, f.eks. på en skala fra 1 (minst viktig) til 5 (mest viktig). Standard: 3"},
					},
					Required: []string{"content"},
				},
			},
		},
		{
			Type: "function",
			Function: Function{
				Name:        "retrieve_relevant_memories",
				Description: "Henter relevante minner fra langtidshukommelsen basert på en spørring eller et tema. Brukes for å hente frem informasjon som kan være relevant for den pågående samtalen.",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]Property{
						"query": {Type: "string", Description: "Søkestrengen eller temaet som skal brukes for å finne relevante minner. Eksempel: brukerens preferanser, tidligere prosjekter nevnt"},
						"count": {Type: "integer", Description: "Maksimalt antall minner som skal hentes. Standard: 3"},
					},
					Required: []string{"query"},
				},
			},
		},
	}
}

func createMemory(argsStr string) (string, error) {
	var args CreateMemoryArgs
	err := json.Unmarshal([]byte(argsStr), &args)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal create_memory arguments: %w", err)
	}

	if args.Content == "" {
		return "", fmt.Errorf("content for memory cannot be empty")
	}

	if args.Type == "" {
		args.Type = "general"
	}
	if args.Importance == 0 {
		args.Importance = 3
	}

	metadataMap := map[string]interface{}{
		"type":       args.Type,
		"importance": args.Importance,
	}
	metadataJSON, err := json.Marshal(metadataMap)
	if err != nil {
		return "", fmt.Errorf("failed to marshal metadata for memory: %w", err)
	}

	stmt, err := db.Prepare("INSERT INTO memories (content, metadata, last_accessed_at) VALUES (?, ?, ?)")
	if err != nil {
		return "", fmt.Errorf("failed to prepare statement for inserting memory: %w", err)
	}
	defer stmt.Close()

	_, err = stmt.Exec(args.Content, string(metadataJSON), time.Now())
	if err != nil {
		return "", fmt.Errorf("failed to execute statement for inserting memory: %w", err)
	}

	log.Printf("Memory created: %s", args.Content)
	return fmt.Sprintf("Successfully created memory with content: %s", args.Content), nil
}

func retrieveRelevantMemories(argsStr string) (string, error) {
	var args RetrieveMemoriesArgs
	err := json.Unmarshal([]byte(argsStr), &args)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal retrieve_relevant_memories arguments: %w", err)
	}

	if args.Query == "" {
		return "", fmt.Errorf("query for retrieving memories cannot be empty")
	}
	if args.Count <= 0 {
		args.Count = 3 
	}

	memories, err := getAutomaticRelevantMemories(args.Query, args.Count) 
	if err != nil {
		return "", fmt.Errorf("error fetching memories via getAutomaticRelevantMemories: %w", err)
	}

	if len(memories) == 0 {
		return "No relevant memories found for your query.", nil
	}

	memoriesJSON, err := json.Marshal(memories)
	if err != nil {
		return "", fmt.Errorf("failed to marshal retrieved memories: %w", err)
	}

	log.Printf("LLM tool retrieved %d memories for query: %s", len(memories), args.Query)
	return string(memoriesJSON), nil
}

func getAutomaticRelevantMemories(userQuery string, count int) ([]Memory, error) {
	if userQuery == "" {
		return []Memory{}, nil 
	}
	if count <= 0 {
		count = 3 
	}

	keywords := strings.Fields(strings.ToLower(userQuery))
	uniqueKeywords := make(map[string]bool)
	var queryParts []string
	for _, kw := range keywords {
		if len(kw) > 2 && !uniqueKeywords[kw] { 
			uniqueKeywords[kw] = true
			sanitizedKw := strings.ReplaceAll(kw, "'", "''") 
			sanitizedKw = strings.ReplaceAll(sanitizedKw, "%", "") 
			sanitizedKw = strings.ReplaceAll(sanitizedKw, "_", "")  
			if sanitizedKw != "" {
			    queryParts = append(queryParts, "content LIKE '%" + sanitizedKw + "%'")
			}
		}
	}

	if len(queryParts) == 0 {
		return []Memory{}, nil 
	}

	sqlQuery := fmt.Sprintf("SELECT id, content, created_at, last_accessed_at, metadata FROM memories WHERE (%s) ORDER BY last_accessed_at DESC, created_at DESC LIMIT ?", strings.Join(queryParts, " OR "))
	
	rows, err := db.Query(sqlQuery, count)
	if err != nil {
		return nil, fmt.Errorf("failed to query memories for automatic retrieval: %w. Query: %s", err, sqlQuery)
	}
	defer rows.Close()

	var memories []Memory
	var idsToUpdate []int64
	for rows.Next() {
		var mem Memory
		var createdAtStr string
		var lastAccessedAtStr sql.NullString

		if err := rows.Scan(&mem.ID, &mem.Content, &createdAtStr, &lastAccessedAtStr, &mem.Metadata); err != nil {
			log.Printf("Error scanning memory row during automatic retrieval: %v", err)
			continue
		}
		mem.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)
		if lastAccessedAtStr.Valid {
			mem.LastAccessedAt.Time, _ = time.Parse("2006-01-02 15:04:05", lastAccessedAtStr.String)
			mem.LastAccessedAt.Valid = true
		}

		memories = append(memories, mem)
		idsToUpdate = append(idsToUpdate, mem.ID)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("memory rows iteration error during automatic retrieval: %w", err)
	}

	if len(idsToUpdate) > 0 {
		tx, err := db.Begin()
		if err != nil {
			return nil, fmt.Errorf("failed to begin transaction for updating access time (auto): %w", err)
		}
		stmt, err := tx.Prepare("UPDATE memories SET last_accessed_at = ? WHERE id = ?")
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("failed to prepare update statement for access time (auto): %w", err)
		}
		defer stmt.Close()
		now := time.Now()
		for _, id := range idsToUpdate {
			if _, err := stmt.Exec(now, id); err != nil {
				tx.Rollback()
				return nil, fmt.Errorf("failed to update access time for memory id %d (auto): %w", id, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("failed to commit transaction for updating access time (auto): %w", err)
		}
	}
	log.Printf("Automatically retrieved %d memories for user query: %s", len(memories), userQuery)
	return memories, nil
}

func callLLM(initialMessages []LLMMessage) (string, error) {
	config, err := getLLMConfig()
	if err != nil {
		return "", fmt.Errorf("error getting LLM config: %w", err)
	}
	llmModel, err := getLLMModel()
	if err != nil {
		return "", fmt.Errorf("error getting LLM model: %w", err)
	}
	apiURL := config.BaseURL
	tools := getTools()

	currentMessages := initialMessages

	for i := 0; i < 5; i++ { 
		requestBody := LLMRequest{
			Model:    llmModel,
			Messages: currentMessages,
			Tools:    tools,
		}
		jsonData, err := json.MarshalIndent(requestBody, "", "  ") 
		if err != nil {
			return "", fmt.Errorf("error marshalling LLM request on iteration %d: %w", i, err)
		}
		// log.Printf("LLM Request (iteration %d):\n%s\n", i, string(jsonData))

		req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
		if err != nil {
			return "", fmt.Errorf("error creating LLM request on iteration %d: %w", i, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+config.APIKey)

		client := &http.Client{Timeout: 120 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("error sending request to LLM on iteration %d: %w", i, err)
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close() 
		if readErr != nil {
			return "", fmt.Errorf("error reading LLM response body on iteration %d: %w", i, readErr)
		}
		// log.Printf("LLM Response Body (iteration %d):\n%s\n", i, string(bodyBytes))

		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("LLM API request failed on iteration %d with status %d: %s", i, resp.StatusCode, string(bodyBytes))
		}

		var llmResponse LLMResponse
		if err := json.Unmarshal(bodyBytes, &llmResponse); err != nil {
			return "", fmt.Errorf("error decoding LLM response on iteration %d: %s. Body: %s", i, err, string(bodyBytes))
		}

		if llmResponse.Error != nil {
			return "", fmt.Errorf("LLM API returned an error on iteration %d: %s (Type: %s)", i, llmResponse.Error.Message, llmResponse.Error.Type)
		}

		if len(llmResponse.Choices) == 0 {
			return "", fmt.Errorf("no choices in LLM response on iteration %d", i)
		}
		choice := llmResponse.Choices[0]
		responseMessage := choice.Message

		currentMessages = append(currentMessages, responseMessage) 

		if len(responseMessage.ToolCalls) > 0 {
			log.Printf("LLM requested tool calls: %+v", responseMessage.ToolCalls)
			for _, toolCall := range responseMessage.ToolCalls {
				var functionResult string
				var funcErr error
				if toolCall.Type == "function" {
					switch toolCall.Function.Name {
					case "create_memory":
						functionResult, funcErr = createMemory(toolCall.Function.Arguments)
					case "retrieve_relevant_memories":
						functionResult, funcErr = retrieveRelevantMemories(toolCall.Function.Arguments)
					default:
						funcErr = fmt.Errorf("unknown function called: %s", toolCall.Function.Name)
					}
				} else {
					funcErr = fmt.Errorf("unknown tool type: %s", toolCall.Type)
				}

				if funcErr != nil {
					log.Printf("Error executing tool %s: %v", toolCall.Function.Name, funcErr)
					functionResult = fmt.Sprintf("Error executing function %s: %s", toolCall.Function.Name, funcErr.Error())
				}
				
				currentMessages = append(currentMessages, LLMMessage{
					ToolCallID: toolCall.ID,
					Role:       "tool",
					Name:       toolCall.Function.Name,
					Content:    functionResult,
				})
			}
		} else if responseMessage.Content != "" {
			return responseMessage.Content, nil 
		} else if choice.FinishReason == "stop" || choice.FinishReason == "length" {
            log.Printf("LLM finished with reason '%s' but no content and no tool_calls. Returning empty.", choice.FinishReason)
            return "", nil 
        } else if choice.FinishReason == "tool_calls" && len(responseMessage.ToolCalls) == 0 {
            log.Printf("LLM indicated finish_reason tool_calls but no tool_calls in message. Continuing loop.")
        } else {
            log.Printf("LLM response has no content and no tool calls. Finish reason: '%s'. Iteration %d", choice.FinishReason, i)
        }
	}
	return "", fmt.Errorf("exceeded maximum iterations for tool calls, or LLM did not provide a final answer")
}

func main() {
	log.Println("DEBUG: main() function started.")
	// The init() function should have already run and set up the database.
	// We can add a check here to be absolutely sure db is not nil.
	if db == nil {
	    log.Fatal("DEBUG: FATAL - Database connection (db) is nil in main(). This should not happen.")
	}
	log.Println("DEBUG: Database connection (db) is not nil in main().")

	fs := http.FileServer(http.Dir("static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		err := tpl.ExecuteTemplate(w, "index.html", nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			log.Println("Error executing template:", err)
		}
	})

	http.HandleFunc("/send_message", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
			return
		}
		userMessageText := r.FormValue("message")
		if userMessageText == "" {
			http.Error(w, "Empty message", http.StatusBadRequest)
			return
		}

		log.Println("Received user message:", userMessageText)
		err := storeMessage("user", userMessageText)
		if err != nil {
			log.Println("Error storing user message:", err)
			http.Error(w, "Failed to store user message", http.StatusInternalServerError)
			return
		}

		llmMessages := []LLMMessage{}
		llmMessages = append(llmMessages, LLMMessage{Role: "system", Content: "You are LISA, a helpful assistant. You can use tools to create memories and retrieve relevant information. I may also provide you with potentially relevant memories from your long-term storage before the conversation history."})

		autoRetrievedMemories, memErr := getAutomaticRelevantMemories(userMessageText, 3) 
		if memErr != nil {
		    log.Printf("Error automatically retrieving memories: %v. Proceeding without them.", memErr)
		} else if len(autoRetrievedMemories) > 0 {
		    var memoryContext strings.Builder
		    memoryContext.WriteString("Relevant information from your long-term memory (most recent first):\n")
		    for i, mem := range autoRetrievedMemories {
		        memoryContext.WriteString(fmt.Sprintf("%d. %s (Stored on: %s)\n", i+1, mem.Content, mem.CreatedAt.Format("2006-01-02")))
		    }
		    llmMessages = append(llmMessages, LLMMessage{Role: "system", Content: strings.TrimSpace(memoryContext.String())})
		    log.Printf("Added %d automatically retrieved memories to prompt.", len(autoRetrievedMemories))
		}

		history, err := getHistory()
		if err != nil {
			log.Printf("Could not get history for LLM call: %v. Proceeding without history.", err)
		} else {
			for _, hMsg := range history {
				llmRole := "user"
				if hMsg.Sender == "lisa" {
					llmRole = "assistant"
				}
				llmMessages = append(llmMessages, LLMMessage{Role: llmRole, Content: hMsg.Message})
			}
		}
		llmMessages = append(llmMessages, LLMMessage{Role: "user", Content: userMessageText})

		lisaResponseText, err := callLLM(llmMessages)
		if err != nil {
			log.Println("Error in LLM processing chain:", err)
			safeErrorMsg := "Beklager, jeg kunne ikke behandle forespørselen din akkurat nå."
			if strings.Contains(err.Error(), "LLM API key or Base URL not configured") {
				safeErrorMsg = "LLM-konfigurasjonsfeil. Vennligst sjekk serverinnstillingene."
			} else if strings.Contains(err.Error(), "LLM API request failed") {
				safeErrorMsg = "Problem med å nå LLM-tjenesten. Prøv igjen senere."
			}
			lisaResponseText = safeErrorMsg
			
			errStore := storeMessage("lisa", lisaResponseText) 
			if errStore != nil {
				log.Println("Error storing LISA error response:", errStore)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"response": lisaResponseText, "sender": "lisa"})
			return
		}

		if lisaResponseText == "" {
		    log.Println("LLM returned an empty final response.")
		    lisaResponseText = "Jeg har behandlet forespørselen din." 
		}

		err = storeMessage("lisa", lisaResponseText)
		if err != nil {
			log.Println("Error storing LISA response:", err)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"response": lisaResponseText, "sender": "lisa"})
	})

	http.HandleFunc("/get_history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
			return
		}
		history, err := getHistory()
		if err != nil {
			log.Println("Error fetching history:", err)
			http.Error(w, "Failed to fetch history", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(history)
	})

	log.Println("DEBUG: Attempting to start webserver on http://0.0.0.0:8080...")
	if err := http.ListenAndServe("0.0.0.0:8080", nil); err != nil {
		log.Fatalf("DEBUG: FATAL - ListenAndServe: %v", err)
	}
}

