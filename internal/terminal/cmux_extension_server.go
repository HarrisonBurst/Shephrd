package terminal

import (
	"fmt"
	"io"
	"os"
)

func RunCmuxExtension(args []string, stdin io.Reader, stdout io.Writer) error {
	return runTerminalExtension(args, stdin, stdout, cmuxExtensionServer(NewCmux))
}

func runCmuxExtensionInvocation(stdin io.Reader, stdout io.Writer) error {
	return runCmuxExtensionInvocationWithClient(stdin, stdout, NewCmux)
}

func runCmuxExtensionInvocationWithClient(stdin io.Reader, stdout io.Writer, client func(string) CmuxClient) error {
	return runTerminalExtensionInvocation(stdin, stdout, cmuxExtensionServer(client))
}

func cmuxExtensionServer(client func(string) CmuxClient) terminalExtensionServer {
	return terminalExtensionServer{
		spec: cmuxExtensionSpec(),
		newClient: func(socketPath string) RuntimeClient {
			return client(socketPath)
		},
		detect: func(context ParentContext) Detection {
			return client("").DetectParent(context)
		},
	}
}

func CmuxExtensionMain() {
	if err := RunCmuxExtension(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
