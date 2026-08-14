package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kenshaw/escpos"
)

func TestEncodeCP437(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []byte
	}{
		{"ascii passes through", "Chicken Popcorn", []byte("Chicken Popcorn")},
		{"pound sign", "£12.99", append([]byte{0x9C}, []byte("12.99")...)},
		{"n with tilde", "jalapeños", []byte{'j', 'a', 'l', 'a', 'p', 'e', 0xA4, 'o', 's'}},
		{"curly apostrophe becomes ascii", "Daniel’s", []byte("Daniel's")},
		{"smart quotes become ascii", "“extra”", []byte(`"extra"`)},
		{"em dash becomes hyphen", "well—done", []byte("well-done")},
		{"ellipsis expands", "wait…", []byte("wait...")},
		{"euro is not a pound", "€5", []byte("EUR5")},
		{"unmappable falls back", "sushi 寿司", []byte("sushi ??")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := encodeCP437(tt.in); !bytes.Equal(got, tt.want) {
				t.Errorf("encodeCP437(%q) = % x, want % x", tt.in, got, tt.want)
			}
		})
	}
}

// Encoded output must be one byte per visible character, otherwise every
// right-aligned column drifts by the extra UTF-8 bytes.
func TestEncodedWidthMatchesRuneCount(t *testing.T) {
	for _, s := range []string{
		"Add jalapeños",
		"Jack Daniel’s Bourbon Sticky BBQ",
		"£12.99",
		"Chicken Popcorn",
	} {
		if got, want := len(encodeCP437(s)), utf8.RuneCountInString(s); got != want {
			t.Errorf("encodeCP437(%q) is %d bytes, want %d to match rune count", s, got, want)
		}
	}
}

func TestPadLineWidth(t *testing.T) {
	for _, tt := range []struct{ label, value string }{
		{"SUBTOTAL", "£19.97"},
		{"TOTAL", "£124.50"},
		{"Add jalapeños", "£0.50"},
	} {
		line := padLine(tt.label, tt.value)
		if got := len(encodeCP437(line)); got != printerWidth {
			t.Errorf("padLine(%q, %q) prints %d chars, want %d: %q", tt.label, tt.value, got, printerWidth, line)
		}
		if !strings.HasSuffix(line, tt.value) {
			t.Errorf("padLine(%q, %q) = %q, value not flushed right", tt.label, tt.value, line)
		}
	}
}

func TestItemLines(t *testing.T) {
	t.Run("short name shares a line with the price", func(t *testing.T) {
		lines := itemLines("Chicken Popcorn", "£8.99")
		if len(lines) != 1 {
			t.Fatalf("got %d lines, want 1: %q", len(lines), lines)
		}
		if got := len(encodeCP437(lines[0])); got != printerWidth {
			t.Errorf("line prints %d chars, want %d: %q", got, printerWidth, lines[0])
		}
	})

	t.Run("long name wraps and right-aligns the price", func(t *testing.T) {
		lines := itemLines("2x Double Bacon Jam Smash (award winning burger)", "£25.98")
		if len(lines) < 2 {
			t.Fatalf("got %d lines, want at least 2: %q", len(lines), lines)
		}
		for _, line := range lines {
			if got := len(encodeCP437(line)); got > printerWidth {
				t.Errorf("line overflows paper at %d chars: %q", got, line)
			}
		}
		if last := lines[len(lines)-1]; !strings.HasSuffix(last, "£25.98") {
			t.Errorf("price line = %q, want it to end with the price", last)
		}
	})
}

func TestWrapText(t *testing.T) {
	t.Run("wraps on word boundaries", func(t *testing.T) {
		lines := wrapText("No mayo, no lettuce, no onions, extra pickles please", 20)
		for _, line := range lines {
			if width(line) > 20 {
				t.Errorf("line exceeds width: %q", line)
			}
		}
		if joined := strings.Join(lines, " "); joined != "No mayo, no lettuce, no onions, extra pickles please" {
			t.Errorf("text lost in wrapping: %q", joined)
		}
	})

	t.Run("hard-splits a word longer than the line", func(t *testing.T) {
		lines := wrapText(strings.Repeat("a", 45), 20)
		if len(lines) != 3 {
			t.Fatalf("got %d lines, want 3: %q", len(lines), lines)
		}
		for _, line := range lines {
			if width(line) > 20 {
				t.Errorf("line exceeds width: %q", line)
			}
		}
	})
}

func TestCollapseModifications(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "distinct selections are kept in order",
			in:   []string{"Buffalo Sauce", "Plain Chips", "Add jalapeños"},
			want: []string{"Buffalo Sauce", "Plain Chips", "Add jalapeños"},
		},
		{
			name: "repeats are counted, not dropped",
			in:   []string{"Add Cheese", "Add Cheese", "Add Cheese"},
			want: []string{"3x Add Cheese"},
		},
		{
			name: "case variants count as the same selection",
			in:   []string{"Buffalo Sauce", "buffalo sauce"},
			want: []string{"2x Buffalo Sauce"},
		},
		{
			name: "blank entries are skipped",
			in:   []string{"", "   ", "Plain Chips"},
			want: []string{"Plain Chips"},
		},
		{
			name: "long comma-separated notes survive",
			in:   []string{"Allergy: nut allergy, dairy allergy, gluten allergy, please be careful"},
			want: []string{"Allergy: nut allergy, dairy allergy, gluten allergy, please be careful"},
		},
		{
			name: "whitespace is normalised before comparing",
			in:   []string{"Add  Cheese", "Add Cheese"},
			want: []string{"2x Add Cheese"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collapseModifications(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParsePrice(t *testing.T) {
	valid := map[string]float64{
		"8.99":   8.99,
		" 8.99 ": 8.99,
		"£8.99":  8.99,
		"0.00":   0,
		"":       0,
		"124.50": 124.50,
	}
	for in, want := range valid {
		got, err := parsePrice(in)
		if err != nil {
			t.Errorf("parsePrice(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parsePrice(%q) = %v, want %v", in, got, want)
		}
	}

	for _, in := range []string{"eight", "8,99", "8.9.9", "--3"} {
		if _, err := parsePrice(in); err == nil {
			t.Errorf("parsePrice(%q) = nil error, want a rejection rather than a silent 0", in)
		}
	}
}

func TestValidate(t *testing.T) {
	valid := ReceiptContent{
		OrderID: "BNAT-1001",
		Items:   []Item{{Name: "Chicken Popcorn", Price: "8.99", Quantity: 1}},
		Total:   "8.99",
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}

	tests := map[string]ReceiptContent{
		"missing order id":  {Items: valid.Items, Total: "8.99"},
		"no items":          {OrderID: "BNAT-1001", Total: "8.99"},
		"item without name": {OrderID: "BNAT-1001", Items: []Item{{Price: "8.99"}}, Total: "8.99"},
		"bad item price":    {OrderID: "BNAT-1001", Items: []Item{{Name: "Chips", Price: "free"}}, Total: "8.99"},
		"bad total":         {OrderID: "BNAT-1001", Items: valid.Items, Total: "lots"},
	}
	for name, receipt := range tests {
		t.Run(name, func(t *testing.T) {
			if err := receipt.validate(); err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}

func TestItemQuantityDefaults(t *testing.T) {
	for _, tt := range []struct{ in, want int }{{0, 1}, {-3, 1}, {1, 1}, {4, 4}} {
		if got := (Item{Quantity: tt.in}).quantity(); got != tt.want {
			t.Errorf("quantity(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// renderToString runs the layout against an in-memory buffer and strips ESC/POS
// control sequences, leaving what the customer would read on the paper.
func renderToString(t *testing.T, receipt ReceiptContent) string {
	t.Helper()
	var buf bytes.Buffer
	render(&receiptPrinter{esc: escpos.New(&buf)}, receipt)

	out := buf.Bytes()
	var text []byte
	for i := 0; i < len(out); i++ {
		switch out[i] {
		case 0x1B: // ESC x n
			i += 2
		case 0x1D: // GS ! n
			i += 2
		case 0x0C: // form feed
		default:
			text = append(text, out[i])
		}
	}
	return string(text)
}

func TestRenderReceipt(t *testing.T) {
	receipt := ReceiptContent{
		OrderID:    "BNAT-1042",
		RiderCode:  "r7yk",
		Restaurant: "Burger Nation",
		Status:     "collection",
		Notes:      "Ring the bell — code is 1234",
		Total:      "24.47",
		Items: []Item{
			{
				Name:          "Chicken Popcorn",
				Price:         "8.99",
				Quantity:      2,
				Modifications: []string{"Buffalo Sauce", "Add jalapeños", "Add Cheese", "Add Cheese"},
			},
			{
				Name:     "Double Bacon Jam Smash (award winning burger)",
				Price:    "1.99",
				Quantity: 1,
			},
		},
	}
	if err := receipt.validate(); err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}

	got := renderToString(t, receipt)

	for _, want := range []string{
		"BURGER NATION",
		"ORDER NUMBER",
		"BNAT-1042",
		"RIDER CODE",
		"R7YK",
		"COLLECTION",
		"2x Chicken Popcorn",
		"2x Add Cheese",   // repeats counted, not collapsed away
		"Buffalo Sauce",   //
		"SUBTOTAL",        //
		"OTHER CHARGES",   // 24.47 total vs 19.97 of items
		"TOTAL",           //
		"NOTES:",          //
		"code is 1234",    //
		"Ring the bell -", // em dash transliterated
	} {
		if !strings.Contains(got, want) {
			t.Errorf("receipt missing %q\n---\n%s\n---", want, got)
		}
	}

	// jalapeños must reach the paper as CP437, not raw UTF-8.
	if !strings.Contains(got, "Add jalape\xa4os") {
		t.Errorf("jalapeños not encoded to CP437\n---\n%q\n---", got)
	}
	if strings.Contains(got, "\xc3\xb1") {
		t.Error("raw UTF-8 leaked to the printer")
	}

	// No line may exceed the paper width.
	for _, line := range strings.Split(got, "\n") {
		if len(line) > printerWidth {
			t.Errorf("line overflows %d chars: %q", printerWidth, line)
		}
	}
}

// Double-width characters occupy two columns each, so a banner that fits the
// 32-char paper as plain text can still run off the edge when enlarged.
func TestBannerFallsBackToNormalSizeWhenTooWide(t *testing.T) {
	const doubleSizeOn = "\x1d\x21\x11"

	render := func(s string) string {
		var buf bytes.Buffer
		(&receiptPrinter{esc: escpos.New(&buf)}).banner(s)
		return buf.String()
	}

	if got := render("BURGER NATION"); !strings.Contains(got, doubleSizeOn) {
		t.Errorf("short banner should print at double size, got %q", got)
	}

	long := "ORDER # BNAT-20260814-1042"
	got := render(long)
	if strings.Contains(got, doubleSizeOn) {
		t.Errorf("banner %q is %d chars, over the %d-char double-width budget, but printed enlarged", long, width(long), bannerWidth)
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > printerWidth {
			t.Errorf("banner line overflows paper: %q", line)
		}
	}
}

func TestRenderOmitsOtherChargesWhenTotalsAgree(t *testing.T) {
	got := renderToString(t, ReceiptContent{
		OrderID: "BNAT-1043",
		Items:   []Item{{Name: "Chips", Price: "1.99", Quantity: 1}},
		Total:   "1.99",
	})
	if strings.Contains(got, "OTHER CHARGES") {
		t.Errorf("unexpected OTHER CHARGES line when totals match\n---\n%s\n---", got)
	}
}

// A failing device must surface as an error rather than a "printed
// successfully" response; escpos discards write errors on its own.
func TestErrWriterCapturesFirstError(t *testing.T) {
	want := errors.New("device disconnected")
	w := &errWriter{w: failingDevice{err: want}}
	p := &receiptPrinter{esc: escpos.New(w)}
	render(p, ReceiptContent{
		OrderID: "BNAT-1044",
		Items:   []Item{{Name: "Chips", Price: "1.99", Quantity: 1}},
		Total:   "1.99",
	})
	if !errors.Is(w.err, want) {
		t.Errorf("errWriter.err = %v, want %v", w.err, want)
	}
}

type failingDevice struct{ err error }

func (d failingDevice) Write([]byte) (int, error) { return 0, d.err }
func (d failingDevice) Read([]byte) (int, error)  { return 0, d.err }
