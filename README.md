# Protobuf Language Server

# Coming Soon™

A language server implementation for Protocol Buffers. Still in development.

Features in progress:
- [x] Semantic token syntax highlighting
- [x] Code formatting in the style of gofmt
- [x] Detailed workspace diagnostics
- [x] Several different import resolution strategies including importing from Go modules
- [ ] LSP features:
  - [x] Go-to-definition
  - [ ] Find references
  - [x] Hover
- Code completion:
  - [x] Message and enum types (with automatic import management)
  - [ ] Import paths 
  - [ ] Message and field literals
- [x] Inlay hints for message and field literal types
- [ ] Code generator tools built in
- [ ] Editor support
  - [x] VSCode
  - [ ] Neovim

Built using modified versions of bufbuild/protocompile, jhump/protoreflect, and golang/tools.
