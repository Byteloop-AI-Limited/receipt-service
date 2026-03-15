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
	Total      string `json:"total"`  // Total amount in pounds
	Status     string `json:"status"` // Delivery, Paid Collection, Unpaid Collection
	Notes      string `json:"notes"`
}

// Item represents each item in the receipt
type Item struct {
	Name          string   `json:"name"`
	Price         string   `json:"price"`         // Price in pounds
	Quantity      int      `json:"quantity"`      // Quantity of the item
	Description   string   `json:"description"`   // Optional: Item description/summary
	Modifications []string `json:"modifications"` // Optional: List of modifications/add-ons
}

func fixPrinterPermissions() error {
	devicePath := "/dev/usb/lp0"
	info, err := os.Stat(devicePath)
	if os.IsNotExist(err) {
		return fmt.Errorf("printer device %s not found", devicePath)
	}

	// Check current permissions
	if info.Mode()&0666 != 0666 {
		// Apply chmod 666
		cmd := exec.Command("sudo", "chmod", "666", devicePath)
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("failed to fix printer permissions: %v", err)
		}
		log.Println("Printer permissions fixed.")
	} else {
		log.Println("Printer permissions are already correct.")
	}
	return nil
}

func printReceipt(receipt ReceiptContent) error {
	// Ensure printer permissions are correct
	if err := fixPrinterPermissions(); err != nil {
		return err
	}

	// Open the printer device
	devicePath := "/dev/usb/lp0"
	file, err := os.OpenFile(devicePath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open printer: %v", err)
	}
	defer file.Close()

	// Create a new escpos printer using the file
	printer := escpos.New(file)

	// Initialize the printer
	printer.Init()

	// Set character encoding to Code Page 858 for '£' symbol
	printer.Write(string([]byte{0x1B, 0x74, 19}))

	// ============================================
	// HEADER SECTION
	// ============================================
	printer.SetAlign("center")
	printer.Write(string([]byte{0x1D, 0x21, 0x11})) // Double height and width
	printer.Write(fmt.Sprintf("%s\n", strings.ToUpper(cleanText(receipt.Restaurant))))
	printer.Write(string([]byte{0x1D, 0x21, 0x00})) // Reset font size
	printer.Write("\n")

	// Separator line
	printer.Write(strings.Repeat("=", 32) + "\n")
	printer.Write("\n")

	// ============================================
	// ORDER INFORMATION
	// ============================================
	printer.SetAlign("left")
	printer.SetEmphasize(1) // Bold
	printer.Write("ORDER #")
	printer.SetEmphasize(0)
	printer.Write(fmt.Sprintf(" %s\n", cleanText(receipt.OrderID)))

	// Date and Time
	orderTime := time.Now()
	printer.Write(fmt.Sprintf("%s\n", orderTime.Format("Mon 02 Jan 2006")))
	printer.Write(fmt.Sprintf("%s\n", orderTime.Format("15:04")))
	printer.Write("\n")

	// Order Type (Status)
	printer.SetAlign("center")
	printer.SetEmphasize(1)
	statusUpper := strings.ToUpper(cleanText(receipt.Status))
	printer.Write(fmt.Sprintf("[ %s ]\n", statusUpper))
	printer.SetEmphasize(0)
	printer.Write("\n")

	// Separator
	printer.Write(strings.Repeat("-", 32) + "\n")
	printer.Write("\n")

	// ============================================
	// ITEMS SECTION
	// ============================================
	printer.SetAlign("left")

	var subtotal float64 = 0

	for i, item := range receipt.Items {
		if i > 0 {
			printer.Write("\n")
		}

		itemPrice := parsePrice(item.Price)
		itemTotal := itemPrice * float64(item.Quantity)
		subtotal += itemTotal

		// Item name with quantity
		printer.SetEmphasize(1) // Bold
		if item.Quantity > 1 {
			printer.Write(fmt.Sprintf("%dx %s\n", item.Quantity, cleanText(item.Name)))
		} else {
			printer.Write(fmt.Sprintf("%s\n", cleanText(item.Name)))
		}
		printer.SetEmphasize(0)

		// Item description if available (only if different from name)
		if item.Description != "" && !strings.EqualFold(strings.TrimSpace(item.Description), strings.TrimSpace(item.Name)) {
			desc := cleanText(item.Description)
			printer.Write(fmt.Sprintf("  %s\n", wrapText(desc, 30)))
		}

		// Process and display modifications/Add-ons (deduplicated and cleaned)
		mods := deduplicateModifications(item.Modifications, item.Description)
		if len(mods) > 0 {
			for _, mod := range mods {
				printer.Write(fmt.Sprintf("  - %s\n", wrapText(mod, 28)))
			}
		}

		// Item price - right aligned
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
	// TOTALS SECTION
	// ============================================
	printer.Write("\n")
	printer.Write(strings.Repeat("-", 32) + "\n")
	printer.Write("\n")

	printer.SetAlign("right")
	printer.Write(fmt.Sprintf("SUBTOTAL %s\n", formatPrice(subtotal)))
	printer.SetAlign("left")
	printer.Write("\n")

	printer.SetAlign("right")
	printer.SetEmphasize(1)
	printer.Write(fmt.Sprintf("TOTAL %s\n", formatPrice(parsePrice(receipt.Total))))
	printer.SetEmphasize(0)
	printer.SetAlign("left")

	printer.Write("\n")
	printer.Write(strings.Repeat("=", 32) + "\n")
	printer.Write("\n")

	// ============================================
	// NOTES SECTION
	// ============================================
	if receipt.Notes != "" && strings.TrimSpace(receipt.Notes) != "" {
		printer.SetAlign("left")
		printer.SetEmphasize(1)
		printer.Write("NOTES:\n")
		printer.SetEmphasize(0)
		printer.Write(wrapText(cleanText(receipt.Notes), 32))
		printer.Write("\n\n")
		printer.Write(strings.Repeat("-", 32) + "\n")
		printer.Write("\n")
	}

	// ============================================
	// FOOTER SECTION
	// ============================================
	printer.SetAlign("center")
	printer.Write("Thank you for your order!\n")
	printer.Write("\n")
	printer.Write("We hope to see you again soon\n")
	printer.Write("\n")

	// Feed and Cut
	printer.FormfeedN(2)
	printer.Cut()
	printer.End()

	return nil
}

// cleanText removes special characters and normalizes text
func cleanText(text string) string {
	// Remove common problematic characters and normalize
	text = strings.TrimSpace(text)
	// Replace multiple spaces with single space
	text = strings.Join(strings.Fields(text), " ")
	// Remove or replace special characters that might cause issues
	text = strings.ReplaceAll(text, "€", "£")
	text = strings.ReplaceAll(text, "  ", " ")
	return text
}

// deduplicateModifications removes duplicate and redundant modifications
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

		// Skip if already seen
		lower := strings.ToLower(cleaned)
		if seen[lower] {
			continue
		}

		// Skip very long descriptions that are likely duplicates of item description
		if len(cleaned) > 80 && strings.Contains(cleaned, ",") {
			// This is likely a full description, skip it
			continue
		}

		// Skip if modification is already covered in the description
		if descLower != "" {
			// Check if this modification is essentially the same as description
			words := strings.Fields(lower)
			if len(words) > 5 {
				// For longer modifications, check if most words appear in description
				matchCount := 0
				for _, word := range words {
					if len(word) > 3 && strings.Contains(descLower, word) {
						matchCount++
					}
				}
				// If more than 60% of words match, it's likely redundant
				if matchCount > len(words)*6/10 {
					continue
				}
			}
		}

		seen[lower] = true
		result = append(result, cleaned)
	}

	return result
}

// wrapText wraps text to a maximum line width
func wrapText(text string, maxWidth int) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}

	var lines []string
	currentLine := ""

	for _, word := range words {
		if len(currentLine)+len(word)+1 <= maxWidth {
			if currentLine != "" {
				currentLine += " " + word
			} else {
				currentLine = word
			}
		} else {
			if currentLine != "" {
				lines = append(lines, currentLine)
			}
			currentLine = word
		}
	}

	if currentLine != "" {
		lines = append(lines, currentLine)
	}

	return strings.Join(lines, "\n")
}

// formatPrice formats a price with the £ symbol using ESC/POS encoding
func formatPrice(price float64) string {
	// Use \x9C which is the £ symbol in Code Page 858 (CP858)
	return fmt.Sprintf("\x9C%.2f", price)
}

// Helper function to parse price strings into float64
func parsePrice(priceStr string) float64 {
	var price float64
	fmt.Sscanf(priceStr, "%f", &price)
	return price
}

func printReceiptHandler(w http.ResponseWriter, r *http.Request) {
	// Decode the JSON payload for the receipt
	var receipt ReceiptContent
	if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		log.Printf("Failed to decode JSON: %v", err)
		return
	}

	// Print the receipt
	if err := printReceipt(receipt); err != nil {
		if strings.Contains(err.Error(), "/dev/usb/lp0 not found") {
			http.Error(w, "Printer is offline", http.StatusInternalServerError)
		} else {
			http.Error(w, fmt.Sprintf("Failed to print receipt: %v", err.Error()), http.StatusInternalServerError)
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
	// Start the HTTP server
	http.HandleFunc("/", authenticate(printReceiptHandler))

	log.Println("Starting server on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
