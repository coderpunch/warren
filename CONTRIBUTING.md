# How to contribute to warren

Thank you for your help. Read this document before you open a pull request.

## 1. Clean-room rule

This rule has no exceptions.

Warren is an independent implementation. It must not contain work that comes from `github.com/open-webui/terminals`.

- Do not read source code from `github.com/open-webui/terminals` or its forks.
- Do not copy that source code.
- Do not use that source code as a model for warren, in the same or a different programming language.

You can use these sources:

- The source code and the HTTP API of Open Terminal (`github.com/open-webui/open-terminal`, MIT license).
- The source code of Open WebUI (`github.com/open-webui/open-webui`, Open WebUI License), to learn which requests it sends to a terminal server. This license is not MIT. Record facts only. Do not copy code.
- The public documentation at `docs.openwebui.com`.

When a change uses a new fact from these sources, record the fact in a document in `docs/`.
Give the source file and the line, or the address of the page.
In the code, refer to the document and the section.

## 2. Names

Do not use "Open WebUI" or "Terminals" in the name of the project, a binary, an image, or a chart.
You can use these words when you describe compatibility.

## 3. License

Warren has the Apache-2.0 license. Section 5 of that license applies to your contribution: you give your contribution with the same license.

Do not add code that has a different license, unless a maintainer agrees first. Keep the license notice of each file that you copy from an open-source project.

The file `internal/openapi/open-terminal.json` comes from Open Terminal and has the MIT license.
The file `internal/openapi/LICENSE.open-terminal` is its license notice. Keep the two files together.

## 4. Before you start

- For a small correction, open a pull request.
- For a new feature or a design change, open an issue first. Describe the problem and your proposal.
- For a security problem, refer to section 9. Do not give the details in a public issue.

## 5. Build and test

You need Go. The file `go.mod` gives the version.

```bash
go build ./...
```

```bash
go test -race ./...
```

The tests need no cluster and no container runtime. They use a simulated Kubernetes client and a simulated Open Terminal server.
The race detector needs a C compiler.

Do these checks before each commit. The CI does these checks. It also builds the container image.

```bash
gofmt -l .
```

```bash
go mod tidy
```

```bash
go vet ./...
```

```bash
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
```

`gofmt -l .` must show no files. `go mod tidy` must not change `go.mod` or `go.sum`.

If you change the `Dockerfile`, or add a directory at the top level, build the image:

```bash
docker build -t warren:ci .
```

### Development mode

In development mode, warren needs no cluster. Warren sends all requests to one Open Terminal server.
There is no isolation between users. Use this mode only on your computer.

Make these three files in a directory that is not in the repository:

- `connection.key`: the connection key. A key must have a minimum of 32 characters.
- `upstream.key`: the API key of the Open Terminal server.
- `policies.yaml`: the policies. Each policy has an `id`, a `name`, and a `scope`. The `scope` is `user`, `chat`, `model`, or `shared`.

```bash
openssl rand -hex 32 > connection.key
```

```bash
openssl rand -hex 32 > upstream.key
```

```yaml
policies:
  - id: dev
    name: Development
    scope: user
```

Start Open Terminal with the key in `upstream.key`. Then start warren:

```bash
go run ./cmd/warren --api-key-file connection.key --policy-file policies.yaml --dev-upstream-url http://127.0.0.1:8000 --dev-upstream-key-file upstream.key
```

Send a request as Open WebUI does:

```bash
curl -H "Authorization: Bearer $(cat connection.key)" -H "X-User-Id: user-1" -H "X-Terminal-Template-Id: dev" http://127.0.0.1:8080/files/cwd
```

## 6. Code rules

1. Keep the trust model. Refer to section 7.
2. Add a test for each change in behaviour. For a correction, add a test that fails without the correction.
3. Changes to authentication, scope resolution, or policies must have unit tests.
4. Do not add a database. Warren keeps its state in Kubernetes labels, annotations, and Secrets.
5. Add a dependency only when it is necessary. Give the reason in the pull request.
6. Write an error message that tells the administrator what to do.
7. Do not write a secret to a log. This includes keys, tokens, and query strings.

## 7. Trust model

Each change must agree with these statements.

- Only the Open WebUI backend and warren have the connection key.
- For sandbox traffic, the identity of the user comes only from the headers that the Open WebUI backend sets. Refer to `docs/protocol.md`, sections 6.1 and 9.3.
- For a management call, the identity comes from the request data that the Open WebUI backend sends. Warren refuses a management call that has an identity header. Refer to `docs/protocol.md`, section 4.6.
- A sandbox is hostile. It runs the code of users and of models.
- A sandbox must never get the connection key, a credential of a user, or the data of a different sandbox.
- Warren examines each response of a sandbox before Open WebUI gets it.

If your change moves a trust boundary, say so in the pull request.

## 8. Documentation

Write all documentation in ASD-STE100 Simplified Technical English. The primary rules are:

- Write short sentences. Use a maximum of 20 words for an instruction and 25 words for a description.
- Give one instruction in each sentence.
- Use the active voice.
- Use the simple tenses: present, past, and future.
- Do not use the `-ing` form of a verb.
- Use one word for one meaning. Do not use a second word for the same thing.

Names of APIs, headers, fields, and products stay as they are.

## 9. Security problems

Do not give the details of a security problem in a public issue.

1. Open the "Security" page of the repository.
2. If the page has a "Report a vulnerability" button, use it.
3. If the page has no such button, open an issue that asks a maintainer for a private contact. Do not put the details of the problem in that issue.

In the report, give the steps that show the problem, and the version or the commit.

## 10. Commits and pull requests

- Make small commits. Each commit must build and must pass the tests.
- Start the commit message with a short summary in the imperative form, for example "Add the idle reaper".
- Explain the reason for the change in the body of the message.
- Make one pull request for one subject.
- In the pull request, describe the change, the tests that you did, and the items that stay open.
- A maintainer merges a pull request after the CI passes and after a review.
