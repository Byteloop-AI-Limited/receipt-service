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

const (
	staticToken  = "Bearer jUo7WsyYySxi71GwieuFPBfbWj8xR6DaXjUHW7gccT1EaX0DCCm3R3qTmJ3FWh2cFRI7jKOCBodFAvp"
	printerWidth = 32
)

type ReceiptContent struct {
	OrderID    string `json:"order_id"`
	Restaurant string `json:"restaurant"`
	Items      []Item `json:"items"`
	Total      string `json:"total"`
	Status     string `json:"status"`
	Notes      string `json:"notes"`
}

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

func poundSign() string {
	return "\x9C"
}

func formatPrice(price float64) string {
	return fmt.Sprintf("%s%.2f", poundSign(), price)
}

// printItemLine prints name left and price right on same line if they fit.
// If name is too long, prints name on its own line then price right-aligned below.
func printItemLine(printer *escpos.Escpos, name, price string) {
	// Need at least 1 space between name and price
	if len(name)+1+len(price) <= printerWidth {
		spaces := printerWidth - len(name) - len(price)
		printer.Write(fmt.Sprintf("%s%s%s\n", name, strings.Repeat(" ", spaces), price))
	} else {
		// Name too long — name on its own line, price right-aligned below
		printer.Write(name + "\n")
		spaces := printerWidth - len(price)
		if spaces < 0 {
			spaces = 0
		}
		printer.Write(fmt.Sprintf("%s%s\n", strings.Repeat(" ", spaces), price))
	}
}

// printLine prints label left and value right — used for SUBTOTAL/TOTAL
func printLine(printer *escpos.Escpos, label, value string) {
	spaces := printerWidth - len(label) - len(value)
	if spaces < 1 {
		spaces = 1
	}
	printer.Write(fmt.Sprintf("%s%s%s\n", label, strings.Repeat(" ", spaces), value))
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

	// CP437 — most reliable for £ on thermal printers
	printer.Write(string([]byte{0x1B, 0x74, 0x00}))

	// ============================================
	// HEADER
	// ============================================
	printer.SetAlign("center")
	printer.SetEmphasize(1)
	printer.Write(string([]byte{0x1D, 0x21, 0x11}))
	printer.Write(fmt.Sprintf("%s\n", strings.ToUpper(cleanText(receipt.Restaurant))))
	printer.Write(string([]byte{0x1D, 0x21, 0x00}))
	printer.SetEmphasize(0)
	printer.Write("\n")
	printer.Write(strings.Repeat("=", printerWidth) + "\n")
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
	printer.Write(strings.Repeat("-", printerWidth) + "\n")
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

		mods := deduplicateModifications(item.Modifications, item.Description)

		hasDescription := item.Description != "" &&
			!strings.EqualFold(strings.TrimSpace(item.Description), strings.TrimSpace(item.Name)) &&
			!isRedundantDescription(cleanText(item.Description), item.Modifications)

		hasExtras := len(mods) > 0 || hasDescription

		var nameLabel string
		if item.Quantity > 1 {
			nameLabel = fmt.Sprintf("%dx %s", item.Quantity, cleanText(item.Name))
		} else {
			nameLabel = cleanText(item.Name)
		}

		priceStr := formatPrice(itemTotal)

		// Always print name and price — smart layout handles long names
		printer.SetEmphasize(1)
		printItemLine(printer, nameLabel, priceStr)
		printer.SetEmphasize(0)

		// @ each price for qty > 1
		if item.Quantity > 1 {
			printer.Write(fmt.Sprintf("@ %s each\n", formatPrice(itemPrice)))
		}

		// Extras below — no duplicate price
		if hasExtras {
			if hasDescription {
				printer.Write(fmt.Sprintf("  %s\n", smartWrapText(cleanText(item.Description), 30)))
			}
			for _, mod := range mods {
				printer.Write(fmt.Sprintf("  - %s\n", smartWrapText(mod, 28)))
			}
		}
	}

	// ============================================
	// TOTALS
	// ============================================
	printer.Write("\n")
	printer.Write(strings.Repeat("-", printerWidth) + "\n")
	printer.Write("\n")

	printLine(printer, "SUBTOTAL", formatPrice(subtotal))
	printer.Write("\n")
	printer.SetEmphasize(1)
	printLine(printer, "TOTAL", formatPrice(parsePrice(receipt.Total)))
	printer.SetEmphasize(0)

	printer.Write("\n")
	printer.Write(strings.Repeat("=", printerWidth) + "\n")
	printer.Write("\n")

	// ============================================
	// NOTES
	// ============================================
	if strings.TrimSpace(receipt.Notes) != "" {
		printer.SetEmphasize(1)
		printer.Write("NOTES:\n")
		printer.SetEmphasize(0)
		printer.Write(wrapText(cleanText(receipt.Notes), printerWidth))
		printer.Write("\n\n")
		printer.Write(strings.Repeat("-", printerWidth) + "\n")
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

func cleanText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.Join(strings.Fields(text), " ")
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
