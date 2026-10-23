package body

func detectConsoleEncoding() string {
	label, _ := detectConsoleEncodingPlatform()
	return label
}
