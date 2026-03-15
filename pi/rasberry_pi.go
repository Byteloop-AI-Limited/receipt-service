package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kenshaw/escpos"
)

const staticToken = "Bearer jUo7WsyYySxi71GwieuFPBfbWj8xR6DaXjUHW7gccT1EaX0DCCm3R3qTmJ3FWh2cFRI7jKOCBodFAvp"

// ReceiptContent represents the receipt data structure
type ReceiptContent struct {
	OrderID    string `json:"order_id"`
	Restaurant string `json:"restaurant"`
	Items      []Item `json:"items"`
	Total      string `json:"total"`
	Status     string `json:"status"`
	Notes      string `json:"notes"`
}

// Item represents each item in the receipt
type Item struct {
	Name          string   `json:"name"`
	Price         string   `json:"price"`
	Quantity      int      `json:"quantity"`
	Description   string   `json:"description"`
	Modifications []string `json:"modifications"`
}

func fixPrinterPermissions() error {
	devicePath := "/dev/usb/lp0"
	info, err := os.Stat(devicePath)
	if os.IsNotExist(err) {
		return fmt.Errorf("printer device %s not found", devicePath)
	}
	if info.Mode()&0666 != 0666 {
		cmd := exec.Command("sudo", "chmod", "666", devicePath)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to fix printer permissions: %v", err)
		}
		log.Println("Printer permissions fixed.")
	} else {
		log.Println("Printer permissions are already correct.")
	}
	return nil
}

// poundSign returns the correct byte sequence for £ on this printer.
// We try Code Page 858 (0x13) where £ lives at 0x9C.
// If your printer still shows wrong character, swap to tryCP437 below.
func poundSign() string {
	return "\x9C"
}

// formatPrice formats a price with the £ symbol
func formatPrice(price float64) string {
	return fmt.Sprintf("%s%.2f", poundSign(), price)
}

func printReceipt(receipt ReceiptContent) error {
	if err := fixPrinterPermissions(); err != nil {
		return err
	}

	devicePath := "/dev/usb/lp0"
	file, err := os.OpenFile(devicePath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open printer: %v", err)
	}
	defer file.Close()

	printer := escpos.New(file)
	printer.Init()

	// ── Code page selection ──────────────────────────────────────────────
	// ESC t 0x00 = CP437 (USA)         £ at 0x9C ✓
	// ESC t 0x02 = CP850 (Multilingual) £ at 0x9C ✓
	// ESC t 0x13 = CP858               £ at 0x9C ✓
	// Try CP437 first — most widely supported on thermal printers
	printer.Write(string([]byte{0x1B, 0x74, 0x00}))

	// ============================================
	// HEADER
	// ============================================
	printer.SetAlign("center")
	printer.SetEmphasize(1)
	printer.Write(string([]byte{0x1D, 0x21, 0x11})) // Double height + width
	printer.Write(fmt.Sprintf("%s\n", strings.ToUpper(cleanText(receipt.Restaurant))))
	printer.Write(string([]byte{0x1D, 0x21, 0x00})) // Reset size
	printer.SetEmphasize(0)
	printer.Write("\n")

	printer.Write(strings.Repeat("=", 32) + "\n")
	printer.Write("\n")

	// ============================================
	// ORDER INFO
	// ============================================
	printer.SetAlign("left")
	printer.SetEmphasize(1)
	printer.Write(fmt.Sprintf("ORDER # %s\n", cleanText(receipt.OrderID)))
	printer.SetEmphasize(0)

	orderTime := time.Now()
	printer.Write(fmt.Sprintf("%s\n", orderTime.Format("Mon 02 Jan 2006")))
	printer.Write(fmt.Sprintf("%s\n", orderTime.Format("15:04")))
	printer.Write("\n")

	// Order type — big bold centred
	printer.SetAlign("center")
	printer.SetEmphasize(1)
	printer.Write(string([]byte{0x1D, 0x21, 0x11}))
	printer.Write(fmt.Sprintf("%s\n", strings.ToUpper(cleanText(receipt.Status))))
	printer.Write(string([]byte{0x1D, 0x21, 0x00}))
	printer.SetEmphasize(0)
	printer.Write("\n")

	printer.Write(strings.Repeat("-", 32) + "\n")
	printer.Write("\n")

	// ============================================
	// ITEMS
	// ============================================
	printer.SetAlign("left")
	var subtotal float64

	for i, item := range receipt.Items {
		if i > 0 {
			printer.Write("\n")
		}

		itemPrice := parsePrice(item.Price)
		itemTotal := itemPrice * float64(item.Quantity)
		subtotal += itemTotal

		// Item name
		printer.SetEmphasize(1)
		if item.Quantity > 1 {
			printer.Write(fmt.Sprintf("%dx %s\n", item.Quantity, cleanText(item.Name)))
		} else {
			printer.Write(fmt.Sprintf("%s\n", cleanText(item.Name)))
		}
		printer.SetEmphasize(0)

		// Description (only if not redundant)
		if item.Description != "" &&
			!strings.EqualFold(strings.TrimSpace(item.Description), strings.TrimSpace(item.Name)) {
			desc := cleanText(item.Description)
			if !isRedundantDescription(desc, item.Modifications) {
				printer.Write(fmt.Sprintf("  %s\n", smartWrapText(desc, 30)))
			}
		}

		// Modifications
		mods := deduplicateModifications(item.Modifications, item.Description)
		for _, mod := range mods {
			printer.Write(fmt.Sprintf("  - %s\n", smartWrapText(mod, 28)))
		}

		// Price — right aligned
		printer.SetAlign("right")
		if item.Quantity > 1 {
			printer.Write(fmt.Sprintf("%s\n", formatPrice(itemTotal)))
			printer.Write(fmt.Sprintf("(%s each)\n", formatPrice(itemPrice)))
		} else {
			printer.Write(fmt.Sprintf("%s\n", formatPrice(itemPrice)))
		}
		printer.SetAlign("left")
	}

	// ============================================
	// TOTALS
	// ============================================
	printer.Write("\n")
	printer.Write(strings.Repeat("-", 32) + "\n")
	printer.Write("\n")

	printer.SetAlign("right")
	printer.Write(fmt.Sprintf("SUBTOTAL  %s\n", formatPrice(subtotal)))
	printer.Write("\n")
	printer.SetEmphasize(1)
	printer.Write(fmt.Sprintf("TOTAL     %s\n", formatPrice(parsePrice(receipt.Total))))
	printer.SetEmphasize(0)
	printer.SetAlign("left")

	printer.Write("\n")
	printer.Write(strings.Repeat("=", 32) + "\n")
	printer.Write("\n")

	// ============================================
	// NOTES
	// ============================================
	if strings.TrimSpace(receipt.Notes) != "" {
		printer.SetEmphasize(1)
		printer.Write("NOTES:\n")
		printer.SetEmphasize(0)
		printer.Write(wrapText(cleanText(receipt.Notes), 32))
		printer.Write("\n\n")
		printer.Write(strings.Repeat("-", 32) + "\n")
		printer.Write("\n")
	}

	// ============================================
	// FOOTER
	// ============================================
	printer.SetAlign("center")
	printer.Write("Thank you for your order!\n")
	printer.Write("\n")
	printer.Write("We hope to see you again soon\n")
	printer.Write("\n")

	printer.FormfeedN(2)
	printer.Cut()
	printer.End()

	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func cleanText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.Join(strings.Fields(text), " ")
	// Keep £ as-is; replace € with £
	text = strings.ReplaceAll(text, "€", poundSign())
	return text
}

func deduplicateModifications(mods []string, description string) []string {
	if len(mods) == 0 {
		return mods
	}
	seen := make(map[string]bool)
	var result []string
	descLower := strings.ToLower(cleanText(description))

	for _, mod := range mods {
		cleaned := strings.TrimSpace(cleanText(mod))
		if cleaned == "" {
			continue
		}
		lower := strings.ToLower(cleaned)
		if seen[lower] {
			continue
		}
		if len(cleaned) > 60 && strings.Count(cleaned, ",") >= 3 {
			continue
		}
		if descLower != "" {
			words := strings.Fields(lower)
			if len(words) > 4 {
				matchCount := 0
				for _, word := range words {
					if len(word) > 2 && strings.Contains(descLower, word) {
						matchCount++
					}
				}
				if matchCount > len(words)/2 {
					continue
				}
			}
		}
		if isItemList(mod, description) {
			continue
		}
		seen[lower] = true
		result = append(result, cleaned)
	}
	return result
}

func isItemList(mod, description string) bool {
	modLower := strings.ToLower(mod)
	descLower := strings.ToLower(description)
	if strings.Count(modLower, ",") >= 2 {
		modWords := strings.Fields(modLower)
		matches := 0
		for _, word := range modWords {
			if len(word) > 2 && strings.Contains(descLower, word) {
				matches++
			}
		}
		return matches > len(modWords)*7/10
	}
	return false
}

func isRedundantDescription(description string, modifications []string) bool {
	if len(modifications) == 0 {
		return false
	}
	descLower := strings.ToLower(description)
	allMods := strings.ToLower(strings.Join(modifications, " "))
	descWords := strings.Fields(descLower)
	matches := 0
	for _, word := range descWords {
		if len(word) > 2 && strings.Contains(allMods, word) {
			matches++
		}
	}
	return matches > len(descWords)*7/10
}

func smartWrapText(text string, maxWidth int) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}
	var lines []string
	currentLine := ""
	for i, word := range words {
		testLine := currentLine
		if testLine != "" {
			testLine += " " + word
		} else {
			testLine = word
		}
		if len(testLine) <= maxWidth {
			currentLine = testLine
		} else {
			if currentLine != "" {
				lines = append(lines, currentLine)
			}
			currentLine = word
		}
		if i == len(words)-1 && currentLine != "" {
			lines = append(lines, currentLine)
		}
	}
	if len(lines) == 0 && currentLine != "" {
		return currentLine
	}
	return strings.Join(lines, "\n")
}

func wrapText(text string, maxWidth int) string {
	return smartWrapText(text, maxWidth)
}

func parsePrice(priceStr string) float64 {
	var price float64
	fmt.Sscanf(priceStr, "%f", &price)
	return price
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

func printReceiptHandler(w http.ResponseWriter, r *http.Request) {
	var receipt ReceiptContent
	if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		log.Printf("Failed to decode JSON: %v", err)
		return
	}
	if err := printReceipt(receipt); err != nil {
		if strings.Contains(err.Error(), "/dev/usb/lp0 not found") {
			http.Error(w, "Printer is offline", http.StatusInternalServerError)
		} else {
			http.Error(w, fmt.Sprintf("Failed to print receipt: %v", err), http.StatusInternalServerError)
		}
		log.Printf("Error printing receipt: %v", err)
		return
	}
	log.Println("Receipt printed successfully!")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Receipt printed successfully!"))
}

func authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		token := r.Header.Get("x-Authorization")
		if token != staticToken {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

func main() {
	http.HandleFunc("/", authenticate(printReceiptHandler))
	log.Println("Starting server on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
