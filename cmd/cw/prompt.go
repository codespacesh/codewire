package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// prompt reads a line of input from the terminal.
func prompt(label string) (string, error) {
	fmt.Print(label)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("interrupted")
	}
	return strings.TrimSpace(scanner.Text()), nil
}

// promptDefault reads a line of input with a default value shown in brackets.
func promptDefault(label, defaultVal string) (string, error) {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", label, defaultVal)
	} else {
		fmt.Printf("%s: ", label)
	}
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("interrupted")
	}
	val := strings.TrimSpace(scanner.Text())
	if val == "" {
		return defaultVal, nil
	}
	return val, nil
}

// promptPassword reads a password without echoing.
func promptPassword(label string) (string, error) {
	fmt.Print(label)
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println() // newline after hidden input
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(password), nil
}

// promptSelect displays a numbered list and returns the selected index.
func promptSelect(label string, options []string) (int, error) {
	fmt.Println(label)
	for i, opt := range options {
		fmt.Printf("  [%d] %s\n", i+1, opt)
	}
	for {
		choice, err := prompt("Select: ")
		if err != nil {
			return 0, err
		}
		for i := range options {
			if choice == fmt.Sprintf("%d", i+1) {
				return i, nil
			}
		}
		fmt.Println("Invalid selection, try again.")
	}
}
