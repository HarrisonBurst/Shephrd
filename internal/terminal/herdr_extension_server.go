package terminal

import (
	"fmt"
	"io"
	"os"
)

func RunHerdrExtension(args []string, stdin io.Reader, stdout io.Writer) error {
	return runTerminalExtension(args, stdin, stdout, herdrExtensionServer(New))
}

func runHerdrExtensionInvocation(stdin io.Reader, stdout io.Writer) error {
	return runHerdrExtensionInvocationWithClient(stdin, stdout, New)
}

func runHerdrExtensionInvocationWithClient(stdin io.Reader, stdout io.Writer, client func(string) Client) error {
	return runTerminalExtensionInvocation(stdin, stdout, herdrExtensionServer(client))
}

func herdrExtensionServer(client func(string) Client) terminalExtensionServer {
	return terminalExtensionServer{
		spec: herdrExtensionSpec(),
		newClient: func(socketPath string) RuntimeClient {
			return client(socketPath)
		},
		detect: func(context ParentContext) Detection {
			return client("").DetectParent(context)
		},
	}
}

func HerdrExtensionMain() {
	if err := RunHerdrExtension(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
