package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"time"
)

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
	Name     string `json:"name"`
	Price    string `json:"price"`    // Price in pounds
	Quantity int    `json:"quantity"` // Quantity of the item
}

func generateReceipt(receipt ReceiptContent) string {
	// Initialize receipt output
	output := ""

	// ESC/POS Initialization
	output += "\x1b@"        // Reset the printer
	output += "\x1b\x33\x00" // Set line spacing to minimum (0 units)

	// Set the character encoding to Code Page 858 for £ symbol
	output += "\x1b\x74\x19"

	// Header: Restaurant name in bold, centered
	output += "\x1b\x61\x01" // Center alignment
	output += "\x1b!\x38"    // Bold and large font
	output += fmt.Sprintf("%s\n", receipt.Restaurant)
	output += "\x1b!\x00" // Reset font size
	output += "================================\n"

	// Order ID
	output += fmt.Sprintf("Order ID: %s\n", receipt.OrderID)
	output += "================================\n"

	// Item Table Header
	output += fmt.Sprintf("%-15s %5s %10s\n", "Item", "Qty", "Amount")
	output += "--------------------------------\n"

	// Items Table Rows
	for _, item := range receipt.Items {
		itemName := truncate(item.Name, 15)                       // Ensure item name fits the column width
		amount := fmt.Sprintf("\x9C%.2f", parsePrice(item.Price)) // Use £ symbol as \x9C
		output += fmt.Sprintf("%-15s %5d %10s\n", itemName, item.Quantity, amount)
	}
	output += "--------------------------------\n"

	// Total Amount
	totalAmount := fmt.Sprintf("\x9C%.2f", parsePrice(receipt.Total)) // Use £ symbol as \x9C
	output += fmt.Sprintf("%-15s %15s\n", "Total:", totalAmount)
	output += "================================\n"

	// Notes
	output += fmt.Sprintf("Notes: %s\n", receipt.Notes)
	output += "================================\n"

	// Status (Bold)
	output += "\x1b!\x08" // Bold font
	output += fmt.Sprintf("Status: %s\n", receipt.Status)
	output += "\x1b!\x00" // Reset font size
	output += "================================\n"

	// Submitted Time (Current Date and Time)
	submittedTime := time.Now().Format("2006-01-02 15:04:05") // Format: YYYY-MM-DD HH:MM:SS
	output += fmt.Sprintf("Submitted: %s\n", submittedTime)
	output += "================================\n"

	// Footer: Thank you message
	output += "\x1b\x61\x01" // Center alignment
	output += "\x1b!\x10"    // Double-width text
	output += "Thank you for your order!\n"
	output += "\x1b!\x00" // Reset font size

	// Extra line feeds for paper space
	output += "\n\n\n\n\n"

	// Feed and Cut
	output += "\x1bJ@"
	output += "\x1dV\x41\x00" // Feed and cut

	return output
}

// Truncate a string to a maximum length with ellipsis
func truncate(input string, maxLength int) string {
	if len(input) > maxLength {
		return input[:maxLength-3] + "..." // Add ellipsis for overflow
	}
	return input
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

	// Generate the receipt text
	receiptText := generateReceipt(receipt)

	// Use the lp command to send the receipt to the printer
	cmd := exec.Command("lp", "-d", "Printer_80", "-o", "raw") // Printer_80 is the printer name
	stdin, err := cmd.StdinPipe()
	if err != nil {
		http.Error(w, "Failed to open pipe to printer", http.StatusInternalServerError)
		log.Printf("Failed to open pipe to printer: %v", err)
		return
	}

	go func() {
		defer stdin.Close()
		// Write the receipt text and add additional flushing commands
		stdin.Write([]byte(receiptText))
		stdin.Write([]byte("\n\n\n\n\n"))   // Extra line feeds to ensure flushing
		time.Sleep(2000 * time.Millisecond) // Delay to ensure all data is processed
	}()

	// Run the command and check for errors
	if err := cmd.Run(); err != nil {
		http.Error(w, "Failed to send receipt to printer", http.StatusInternalServerError)
		log.Printf("Failed to send receipt to printer: %v", err)
		return
	}

	log.Println("Receipt printed successfully!")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Receipt printed successfully!"))
}

func main() {
	http.HandleFunc("/print-receipt", printReceiptHandler)

	log.Println("Starting server on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
