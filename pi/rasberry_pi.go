// Command receipt-service exposes a single HTTP endpoint that renders an order
// as an ESC/POS receipt on a thermal printer attached to the Raspberry Pi.
//
// The printer is a shared, single-threaded resource: prints are serialised so
// two concurrent orders cannot interleave their byte streams on the device.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/kenshaw/escpos"
)

const (
	// printerWidth is the character width of the paper at the default font.
	printerWidth = 32

	// defaultDevicePath is the USB printer character device. Override with
	// PRINTER_DEVICE when the Pi enumerates it elsewhere (e.g. /dev/usb/lp1).
	defaultDevicePath = "/dev/usb/lp0"

	// defaultToken is the fallback shared secret. It must match the payment
	// service's ServiceToken. Prefer setting RECEIPT_SERVICE_TOKEN instead of
	// relying on this baked-in value.
	defaultToken = "Bearer jUo7WsyYySxi71GwieuFPBfbWj8xR6DaXjUHW7gccT1EaX0DCCm3R3qTmJ3FWh2cFRI7jKOCBodFAvp"

	// maxBodyBytes caps the request body; a receipt payload is a few KB at most.
	maxBodyBytes = 1 << 20
)

// printMu serialises access to the printer device.
var printMu sync.Mutex

type ReceiptContent struct {
	OrderID    string `json:"order_id"`
	RiderCode  string `json:"rider_code"`
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

// errPrinterOffline reports that the printer device is absent, so the caller
// can fall back to emailing the receipt instead of retrying.
var errPrinterOffline = errors.New("printer offline")

// validate rejects a payload before any paper is used, so a malformed order
// cannot produce a half-printed receipt.
func (r ReceiptContent) validate() error {
	if strings.TrimSpace(r.OrderID) == "" {
		return errors.New("order_id is required")
	}
	if len(r.Items) == 0 {
		return errors.New("at least one item is required")
	}
	for i, item := range r.Items {
		if strings.TrimSpace(item.Name) == "" {
			return fmt.Errorf("item %d: name is required", i+1)
		}
		if _, err := parsePrice(item.Price); err != nil {
			return fmt.Errorf("item %d (%s): %w", i+1, item.Name, err)
		}
	}
	if _, err := parsePrice(r.Total); err != nil {
		return fmt.Errorf("total: %w", err)
	}
	return nil
}

// parsePrice reads a decimal amount. An empty value means zero; anything else
// that will not parse is an error rather than a silent 0.00 on the receipt.
func parsePrice(s string) (float64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "£")
	s = strings.TrimPrefix(s, "GBP")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	price, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid price %q", s)
	}
	return price, nil
}

// quantity treats a missing or nonsensical count as a single unit; the kitchen
// is better served by one of an item than by a rejected order.
func (i Item) quantity() int {
	if i.Quantity < 1 {
		return 1
	}
	return i.Quantity
}

// ---------------------------------------------------------------------------
// Character encoding
// ---------------------------------------------------------------------------

// The printer is initialised to code page 437, so UTF-8 must be translated
// before it reaches the device. Writing raw UTF-8 renders multi-byte runes as
// several garbage glyphs and throws column alignment out by the extra bytes.

// cp437 maps the runes that appear in menu text and customer notes onto their
// code page 437 byte. Runes absent from both tables print as '?'.
var cp437 = map[rune]byte{
	'Ç': 0x80, 'ü': 0x81, 'é': 0x82, 'â': 0x83, 'ä': 0x84, 'à': 0x85, 'å': 0x86,
	'ç': 0x87, 'ê': 0x88, 'ë': 0x89, 'è': 0x8A, 'ï': 0x8B, 'î': 0x8C, 'ì': 0x8D,
	'Ä': 0x8E, 'Å': 0x8F, 'É': 0x90, 'æ': 0x91, 'Æ': 0x92, 'ô': 0x93, 'ö': 0x94,
	'ò': 0x95, 'û': 0x96, 'ù': 0x97, 'ÿ': 0x98, 'Ö': 0x99, 'Ü': 0x9A, '¢': 0x9B,
	'£': 0x9C, '¥': 0x9D, 'ƒ': 0x9F, 'á': 0xA0, 'í': 0xA1, 'ó': 0xA2, 'ú': 0xA3,
	'ñ': 0xA4, 'Ñ': 0xA5, 'ª': 0xA6, 'º': 0xA7, '¿': 0xA8, '¬': 0xAA, '½': 0xAB,
	'¼': 0xAC, '¡': 0xAD, '«': 0xAE, '»': 0xAF, 'ß': 0xE1, 'µ': 0xE6, '±': 0xF1,
	'÷': 0xF6, '°': 0xF8, '·': 0xFA, '²': 0xFD,
}

// translit rewrites runes with no code page 437 equivalent into ASCII that
// reads the same. Applied before the cp437 lookup.
var translit = map[rune]string{
	'‘': "'", '’': "'", '‚': ",", '‛': "'",
	'“': `"`, '”': `"`, '„': `"`, '‟': `"`,
	'–': "-", '—': "-", '‒': "-", '−': "-",
	'…': "...", '•': "*", '™': "(TM)", '©': "(C)", '®': "(R)",
	'€': "EUR", ' ': " ", '​': "",
}

// encodeCP437 converts s into code page 437 bytes. The result has one byte per
// visible character, so byte length and printed width agree.
func encodeCP437(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r < utf8.RuneSelf {
			out = append(out, byte(r))
			continue
		}
		if sub, ok := translit[r]; ok {
			for _, sr := range sub {
				if sr < utf8.RuneSelf {
					out = append(out, byte(sr))
				} else if b, ok := cp437[sr]; ok {
					out = append(out, b)
				}
			}
			continue
		}
		if b, ok := cp437[r]; ok {
			out = append(out, b)
			continue
		}
		out = append(out, '?')
	}
	return out
}

// cleanText collapses whitespace so a name spanning several source lines does
// not blow up the receipt layout.
func cleanText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// width reports the printed width of s in characters, not bytes.
func width(s string) int {
	return utf8.RuneCountInString(s)
}

// ---------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------

// padLine lays label and value out on one line with value flushed right.
func padLine(label, value string) string {
	gap := printerWidth - width(label) - width(value)
	if gap < 1 {
		gap = 1
	}
	return label + strings.Repeat(" ", gap) + value
}

// itemLines renders a name and its price, dropping the price onto its own
// right-aligned line when the pair will not fit the paper width.
func itemLines(name, price string) []string {
	if width(name)+1+width(price) <= printerWidth {
		return []string{padLine(name, price)}
	}
	lines := wrapText(name, printerWidth)
	gap := printerWidth - width(price)
	if gap < 0 {
		gap = 0
	}
	return append(lines, strings.Repeat(" ", gap)+price)
}

// wrapText breaks s onto lines of at most maxWidth characters, splitting on
// word boundaries and hard-splitting any single word that cannot fit.
func wrapText(s string, maxWidth int) []string {
	if maxWidth < 1 {
		return []string{s}
	}
	var lines []string
	var line string
	for _, word := range strings.Fields(s) {
		for width(word) > maxWidth {
			runes := []rune(word)
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			lines = append(lines, string(runes[:maxWidth]))
			word = string(runes[maxWidth:])
		}
		switch {
		case line == "":
			line = word
		case width(line)+1+width(word) <= maxWidth:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// collapseModifications merges identical selections while preserving how many
// were chosen, so three portions of an extra print as "3x Add Cheese" rather
// than silently becoming one. Order of first appearance is kept.
//
// Nothing else is discarded: a modification may carry an allergy warning, and
// dropping it because of its length or punctuation is not a risk worth taking.
func collapseModifications(mods []string) []string {
	type entry struct {
		label string
		count int
	}
	var order []string
	counts := make(map[string]*entry, len(mods))

	for _, mod := range mods {
		cleaned := cleanText(mod)
		if cleaned == "" {
			continue
		}
		key := strings.ToLower(cleaned)
		if e, ok := counts[key]; ok {
			e.count++
			continue
		}
		counts[key] = &entry{label: cleaned, count: 1}
		order = append(order, key)
	}

	result := make([]string, 0, len(order))
	for _, key := range order {
		e := counts[key]
		if e.count > 1 {
			result = append(result, fmt.Sprintf("%dx %s", e.count, e.label))
			continue
		}
		result = append(result, e.label)
	}
	return result
}

// showDescription reports whether a description adds anything beyond the name.
func showDescription(item Item) bool {
	desc := cleanText(item.Description)
	return desc != "" && !strings.EqualFold(desc, cleanText(item.Name))
}

// ---------------------------------------------------------------------------
// Printer
// ---------------------------------------------------------------------------

// errWriter records the first write failure. escpos.Escpos discards the error
// from every write, so without this a disconnected or jammed printer would
// still report a successful receipt.
type errWriter struct {
	w   io.ReadWriter
	err error
}

func (w *errWriter) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.w.Write(b)
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *errWriter) Read(b []byte) (int, error) { return w.w.Read(b) }

// receiptPrinter wraps escpos with the handful of operations this layout needs,
// encoding every string for the printer's code page on the way out.
type receiptPrinter struct {
	esc *escpos.Escpos
}

func (p *receiptPrinter) raw(b ...byte)     { p.esc.WriteRaw(b) }
func (p *receiptPrinter) feed()             { p.esc.WriteRaw([]byte("\n")) }
func (p *receiptPrinter) align(a string)    { p.esc.SetAlign(a) }
func (p *receiptPrinter) rule(char string)  { p.line(strings.Repeat(char, printerWidth)) }
func (p *receiptPrinter) emphasize(on bool) { p.esc.SetEmphasize(boolToUint8(on)) }

// doubleSize toggles double width and height via GS ! n.
func (p *receiptPrinter) doubleSize(on bool) {
	size := byte(0x00)
	if on {
		size = 0x11
	}
	p.raw(0x1D, 0x21, size)
}

// line writes s followed by a newline, encoded for the printer.
func (p *receiptPrinter) line(s string) {
	p.esc.WriteRaw(append(encodeCP437(s), '\n'))
}

// lines writes each element on its own line.
func (p *receiptPrinter) lines(ss []string) {
	for _, s := range ss {
		p.line(s)
	}
}

// bannerWidth is the character budget for double-width text: each character
// occupies two columns, so only half the paper width is available.
const bannerWidth = printerWidth / 2

// banner writes s centred and bold, at double size when it fits. Text too long
// for the double-width budget drops to normal size rather than overflowing the
// paper, which the printer would otherwise wrap mid-word.
func (p *receiptPrinter) banner(s string) {
	p.align("center")
	p.emphasize(true)

	if width(s) <= bannerWidth {
		p.doubleSize(true)
		p.line(s)
		p.doubleSize(false)
	} else {
		p.lines(wrapText(s, printerWidth))
	}

	p.emphasize(false)
	p.feed()
}

// labelledBanner prints a small centred caption above a large centred value.
// Keeping the caption out of the enlarged line leaves the whole double-width
// budget for the value, so order numbers and rider codes stay big and legible.
func (p *receiptPrinter) labelledBanner(label, value string) {
	p.align("center")
	p.line(label)
	p.banner(value)
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// openPrinter opens the printer device, repairing its permissions once if that
// is what stands in the way.
//
// The durable fix is a udev rule or adding the service user to the lp group;
// the chmod here is a fallback and needs passwordless sudo to succeed.
func openPrinter(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err == nil {
		return file, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: device %s not found", errPrinterOffline, path)
	}
	if !errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("failed to open printer %s: %w", path, err)
	}

	log.Printf("Printer %s not writable, attempting to fix permissions", path)
	if chmodErr := exec.Command("sudo", "-n", "chmod", "666", path).Run(); chmodErr != nil {
		return nil, fmt.Errorf("failed to open printer %s (%v) and could not fix permissions: %w", path, err, chmodErr)
	}
	file, err = os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open printer %s after fixing permissions: %w", path, err)
	}
	return file, nil
}

func devicePath() string {
	if path := os.Getenv("PRINTER_DEVICE"); path != "" {
		return path
	}
	return defaultDevicePath
}

// printReceipt renders receipt on the thermal printer. Calls are serialised;
// only one receipt is on the device at a time.
func printReceipt(receipt ReceiptContent) error {
	printMu.Lock()
	defer printMu.Unlock()

	file, err := openPrinter(devicePath())
	if err != nil {
		return err
	}
	defer file.Close()

	dst := &errWriter{w: file}
	p := &receiptPrinter{esc: escpos.New(dst)}

	p.esc.Init()
	p.raw(0x1B, 0x74, 0x00) // select code page 437, matching encodeCP437

	render(p, receipt)

	if dst.err != nil {
		return fmt.Errorf("failed writing to printer: %w", dst.err)
	}
	return nil
}

// render writes the receipt layout. It is separated from device handling so it
// can be exercised against an in-memory buffer in tests.
func render(p *receiptPrinter, receipt ReceiptContent) {
	// Header
	p.banner(strings.ToUpper(cleanText(receipt.Restaurant)))
	p.rule("=")
	p.feed()

	// Order identity
	p.labelledBanner("ORDER NUMBER", cleanText(receipt.OrderID))
	if code := cleanText(receipt.RiderCode); code != "" {
		p.labelledBanner("RIDER CODE", strings.ToUpper(code))
	}

	orderTime := time.Now()
	p.align("center")
	p.line(orderTime.Format("Mon 02 Jan 2006"))
	p.line(orderTime.Format("15:04"))
	p.feed()

	if status := cleanText(receipt.Status); status != "" {
		p.banner(strings.ToUpper(status))
	}
	p.rule("-")
	p.feed()

	// Items
	p.align("left")
	var subtotal float64
	for i, item := range receipt.Items {
		if i > 0 {
			p.feed()
		}

		qty := item.quantity()
		unitPrice, _ := parsePrice(item.Price) // validated before printing
		lineTotal := unitPrice * float64(qty)
		subtotal += lineTotal

		name := cleanText(item.Name)
		if qty > 1 {
			name = fmt.Sprintf("%dx %s", qty, name)
		}

		p.emphasize(true)
		p.lines(itemLines(name, formatPrice(lineTotal)))
		p.emphasize(false)

		if qty > 1 {
			p.line(fmt.Sprintf("@ %s each", formatPrice(unitPrice)))
		}
		if showDescription(item) {
			for _, line := range wrapText(cleanText(item.Description), printerWidth-2) {
				p.line("  " + line)
			}
		}
		for _, mod := range collapseModifications(item.Modifications) {
			for j, line := range wrapText(mod, printerWidth-4) {
				prefix := "  - "
				if j > 0 {
					prefix = "    "
				}
				p.line(prefix + line)
			}
		}
	}

	// Totals
	p.feed()
	p.rule("-")
	p.feed()

	total, _ := parsePrice(receipt.Total) // validated before printing
	p.line(padLine("SUBTOTAL", formatPrice(subtotal)))

	// The caller's total includes service, delivery and small-order fees that
	// are not itemised here; show the difference so the receipt reconciles.
	if diff := total - subtotal; diff > 0.005 || diff < -0.005 {
		p.line(padLine("OTHER CHARGES", formatPrice(diff)))
	}

	p.feed()
	p.emphasize(true)
	p.line(padLine("TOTAL", formatPrice(total)))
	p.emphasize(false)
	p.feed()
	p.rule("=")
	p.feed()

	// Notes
	if notes := cleanText(receipt.Notes); notes != "" {
		p.align("left")
		p.emphasize(true)
		p.line("NOTES:")
		p.emphasize(false)
		p.lines(wrapText(notes, printerWidth))
		p.feed()
		p.rule("-")
		p.feed()
	}

	// Footer
	p.align("center")
	p.line("Thank you for your order!")
	p.feed()
	p.line("We hope to see you again soon")
	p.feed()

	p.esc.FormfeedN(2)
	p.esc.Cut()
	p.esc.End()
}

// formatPrice renders an amount with the code page 437 pound sign.
func formatPrice(price float64) string {
	return fmt.Sprintf("£%.2f", price)
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func printReceiptHandler(w http.ResponseWriter, r *http.Request) {
	var receipt ReceiptContent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&receipt); err != nil {
		log.Printf("Failed to decode JSON: %v", err)
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	if err := receipt.validate(); err != nil {
		log.Printf("Rejected receipt for order %q: %v", receipt.OrderID, err)
		http.Error(w, fmt.Sprintf("Invalid receipt: %v", err), http.StatusBadRequest)
		return
	}

	if err := printReceipt(receipt); err != nil {
		log.Printf("Error printing receipt for order %s: %v", receipt.OrderID, err)
		if errors.Is(err, errPrinterOffline) {
			http.Error(w, "Printer is offline", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to print receipt: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Receipt printed for order %s", receipt.OrderID)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("Receipt printed successfully!")); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}

func authToken() string {
	if token := os.Getenv("RECEIPT_SERVICE_TOKEN"); token != "" {
		return token
	}
	return defaultToken
}

func authenticate(next http.HandlerFunc) http.HandlerFunc {
	expected := []byte(authToken())
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		token := []byte(r.Header.Get("X-Authorization"))
		if subtle.ConstantTimeCompare(token, expected) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// healthHandler reports liveness without requiring the shared secret, so the
// tunnel and any uptime check can probe the service safely.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat(devicePath()); err != nil {
		http.Error(w, "printer unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/", authenticate(printReceiptHandler))

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Shut down cleanly on the signals systemd sends, so an in-progress
	// receipt is not cut off mid-print during a restart.
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdown
		log.Println("Shutting down...")
		printMu.Lock()
		defer printMu.Unlock()
		if err := srv.Close(); err != nil {
			log.Printf("Error during shutdown: %v", err)
		}
	}()

	log.Printf("Starting server on %s (printer %s)", srv.Addr, devicePath())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server failed: %v", err)
	}
}
