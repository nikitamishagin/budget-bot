package llmparser

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLLMParser(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "LLM Parser Suite")
}

var _ = Describe("parseExpensesJSON", func() {
	It("parses a plain JSON object as a single-item list", func() {
		content := `{"type":"expense","amount":12.5,"currency":"USD","bank_account":"1234","date":"2024-01-01","description":"Coffee Shop","parse_failed":false}`

		expenses, err := parseExpensesJSON(content)

		Expect(err).NotTo(HaveOccurred())
		Expect(expenses).To(HaveLen(1))
		Expect(expenses[0].Type).To(Equal(TransactionExpense))
		Expect(expenses[0].Amount).To(Equal(12.5))
		Expect(expenses[0].Currency).To(Equal("USD"))
		Expect(expenses[0].Description).To(Equal("Coffee Shop"))
		Expect(expenses[0].ParseFailed).To(BeFalse())
	})

	It("parses JSON wrapped in markdown code fences and extra text", func() {
		content := "Here is the result:\n```json\n[{\"type\":\"income\",\"amount\":100,\"currency\":\"EUR\",\"bank_account\":\"\",\"date\":\"\",\"description\":\"Salary\",\"parse_failed\":false}]\n```\nDone."

		expenses, err := parseExpensesJSON(content)

		Expect(err).NotTo(HaveOccurred())
		Expect(expenses).To(HaveLen(1))
		Expect(expenses[0].Type).To(Equal(TransactionIncome))
		Expect(expenses[0].Amount).To(Equal(100.0))
		Expect(expenses[0].Description).To(Equal("Salary"))
	})

	It("parses a JSON array with several SMS messages", func() {
		content := `[{"type":"expense","amount":10,"currency":"USD","bank_account":"1111","date":"2024-01-01","description":"Shop A","parse_failed":false},{"type":"expense","amount":20,"currency":"USD","bank_account":"2222","date":"2024-01-02","description":"Shop B","parse_failed":false}]`

		expenses, err := parseExpensesJSON(content)

		Expect(err).NotTo(HaveOccurred())
		Expect(expenses).To(HaveLen(2))
		Expect(expenses[0].Description).To(Equal("Shop A"))
		Expect(expenses[1].Description).To(Equal("Shop B"))
	})

	It("returns an error when no JSON object or array is present", func() {
		_, err := parseExpensesJSON("no json here")

		Expect(err).To(HaveOccurred())
	})

	It("returns an error for malformed JSON", func() {
		_, err := parseExpensesJSON("{not valid json}")

		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("mimeTypeForPath", func() {
	It("detects jpeg", func() {
		Expect(mimeTypeForPath("photo.jpg")).To(Equal("image/jpeg"))
		Expect(mimeTypeForPath("photo.JPEG")).To(Equal("image/jpeg"))
	})

	It("detects png", func() {
		Expect(mimeTypeForPath("photo.png")).To(Equal("image/png"))
	})

	It("defaults to jpeg for unknown extensions", func() {
		Expect(mimeTypeForPath("photo.unknown")).To(Equal("image/jpeg"))
	})
})
