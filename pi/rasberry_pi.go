package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/kenshaw/escpos"
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
	devicePath := "/dev/usb/lp0" // Replace with your printer's device path
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

	// Header: Restaurant name in bold, centered
	printer.SetAlign("center")
	printer.Write(string([]byte{0x1D, 0x21, 0x01})) // Larger font for header
	printer.Write(fmt.Sprintf("%s\n", receipt.Restaurant))
	printer.Write(string([]byte{0x1D, 0x21, 0x00})) // Reset font size
	printer.Write("--------------------------------\n")

	// Order ID (Centered and Adjusted Font)
	printer.SetAlign("center")
	printer.Write(string([]byte{0x1D, 0x21, 0x00})) // Reset to default font size
	printer.Write(fmt.Sprintf("Order ID: %s\n", receipt.OrderID))
	printer.Write("--------------------------------\n")

	// Main Content with Center Alignment
	printer.Write(string([]byte{0x1D, 0x21, 0x00})) // Default font size

	// Item Table Header
	printer.Write(fmt.Sprintf("%-20s %5s %10s\n", "Item", "Qty", "Amount")) // Adjust column widths
	printer.Write("--------------------------------\n")

	// Items Table Rows
	for _, item := range receipt.Items {
		itemName := truncate(item.Name, 20)                       // Ensure item name fits the column width
		amount := fmt.Sprintf("\x9C%.2f", parsePrice(item.Price)) // Use \x9C for £
		printer.Write(fmt.Sprintf("%-20s %5d %10s\n", itemName, item.Quantity, amount))
	}
	printer.Write("--------------------------------\n")

	// Total Amount
	totalAmount := fmt.Sprintf("\x9C%.2f", parsePrice(receipt.Total)) // Use \x9C for £
	printer.Write(fmt.Sprintf("%-20s %15s\n", "Total:", totalAmount))
	printer.Write("--------------------------------\n")

	// Notes
	printer.Write(fmt.Sprintf("Notes: %s\n", receipt.Notes))
	printer.Write("--------------------------------\n")

	// Status (Bold and Centered)
	printer.SetAlign("center")
	printer.SetEmphasize(1) // Bold text
	printer.Write(fmt.Sprintf("Status: %s\n", receipt.Status))
	printer.SetEmphasize(0) // Reset emphasis
	printer.Write("--------------------------------\n")

	// Submitted Time (Current Date and Time)
	submittedTime := time.Now().Format("2006-01-02 15:04:05") // Format: YYYY-MM-DD HH:MM:SS
	printer.Write(fmt.Sprintf("Submitted: %s\n", submittedTime))
	printer.Write("--------------------------------\n")

	// Footer: Thank you message
	printer.SetAlign("center")
	printer.Write(string([]byte{0x1D, 0x21, 0x01})) // Larger font for footer
	printer.Write("Thank you for your order!\n")
	printer.Write(string([]byte{0x1D, 0x21, 0x00})) // Reset font size

	// Feed and Cut
	printer.FormfeedN(3) // Feed paper
	printer.Cut()
	printer.End()

	return nil
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

	// Print the receipt
	if err := printReceipt(receipt); err != nil {
		http.Error(w, "Failed to print receipt", http.StatusInternalServerError)
		log.Printf("Error printing receipt: %v", err)
		return
	}

	log.Println("Receipt printed successfully!")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Receipt printed successfully!"))
}

func main() {
	// Start the HTTP server
	http.HandleFunc("/", printReceiptHandler)

	log.Println("Starting server on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
