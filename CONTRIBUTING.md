# How to contribute to warren

Thank you for your help. Read this document before you open a pull request.

## 1. Clean-room rule

This rule has no exceptions.

Warren is an independent implementation. It must not contain work that comes from `github.com/open-webui/terminals`.

- Do not read source code from `github.com/open-webui/terminals` or its forks.
- Do not copy that source code.
- Do not change that source code for use in warren.
- If you read that source code in the past, do not contribute to warren.

You can use these sources:

- The source code and the HTTP API of Open Terminal (`github.com/open-webui/open-terminal`, MIT license).
- The source code of Open WebUI, to learn which requests it sends to a terminal server. Record facts only. Do not copy code.
- The public documentation at `docs.openwebui.com`.

When a change uses a fact from one of these sources, give the file and the line in `docs/protocol.md` or `docs/open-terminal-api.md`.

## 2. Names

Do not use "Open WebUI" or "Terminals" in the name of the project, a binary, an image, or a chart.
You can use these words when you describe compatibility.

## 3. License

Warren has the Apache-2.0 license. Section 5 of that license applies to your contribution: you give your contribution with the same license.

Do not add code that has a different license, unless a maintainer agrees first. Keep the license notice of each file that you copy from an open-source project.

## 4. Before you start

- For a small correction, open a pull request.
- For a new function or a design change, open an issue first. Describe the problem and your proposal.
- For a security problem, refer to section 9. Do not open a public issue.

## 5. Build and test

You need Go. The file `go.mod` gives the version.

```bash
go build ./...
```

```bash
go test -race ./...
```

Do these checks before each commit. The CI does the same checks.

```bash
gofmt -l .
```

```bash
go vet ./...
```

```bash
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
```

`gofmt -l .` must show no files.

## 6. Code rules

1. Keep the trust model. Refer to section 7.
2. Add a test for each change in behaviour. For a correction, add a test that fails without the correction.
3. Changes to authentication, scope resolution, or policies must have unit tests.
4. Do not add a database. Warren keeps its state in Kubernetes labels, annotations, and Secrets.
5. Add a dependency only when it is necessary. Give the reason in the pull request.
6. Write an error message that tells the operator what to do.
7. Do not write a secret to a log. This includes keys, tokens, and query strings.

## 7. Trust model

Each change must agree with these statements.

- Only the Open WebUI backend has the connection key.
- The identity of the user comes only from the headers that the Open WebUI backend sets. Refer to `docs/protocol.md`, section 6.
- A sandbox is hostile. It runs the code of users and of models.
- A sandbox must never get the connection key, a credential of a user, or the data of a different sandbox.
- Warren examines each response of a sandbox before Open WebUI gets it.

If your change moves a trust boundary, say so in the pull request.

## 8. Documentation

Write all documentation in ASD-STE100 Simplified Technical English. The primary rules are:

- Write short sentences. Use a maximum of 20 words for an instruction and 25 words for a description.
- Give one instruction in each sentence.
- Use the active voice.
- Use the simple present tense.
- Use one word for one meaning. Do not use a second word for the same thing.

Names of APIs, headers, fields, and products stay as they are.

## 9. Security problems

Do not open a public issue for a security problem.
Use the "Report a vulnerability" function on the "Security" page of the repository.
Give the steps that show the problem, and the version or commit.

## 10. Commits and pull requests

- Make small commits. Each commit must build and must pass the tests.
- Start the commit message with a short summary in the imperative form, for example "Add the idle reaper".
- Explain the reason for the change in the body of the message.
- Make one pull request for one subject.
- In the pull request, describe the change, the tests that you did, and the items that stay open.
- A maintainer merges a pull request after the CI passes and after a review.
