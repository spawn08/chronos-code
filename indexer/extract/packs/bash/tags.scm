; Definitions.
(function_definition name: (word) @name) @def.func
(program
  (declaration_command "readonly" (variable_assignment name: (variable_name) @name) @def.const))
(program (declaration_command (variable_assignment name: (variable_name) @name) @def.var))
(program (variable_assignment name: (variable_name) @name) @def.var)

; source and . read another script.
(command
  name: (command_name (word) @_cmd)
  argument: [(word) (string) (raw_string)] @import.path
  (#eq? @_cmd "source")) @import
(command
  name: (command_name (word) @_cmd)
  argument: [(word) (string) (raw_string)] @import.path
  (#eq? @_cmd ".")) @import

; Every command is a call; functions resolve by name.
(command name: (command_name (word) @name)) @ref.call
