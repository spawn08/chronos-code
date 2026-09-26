; Definitions.
(class_declaration name: (identifier) @name body: (_) @body) @def.class
(method_definition
  name: (property_identifier) @name
  (#eq? @name "constructor")) @def.constructor
(method_definition name: (_) @name) @def.method
(field_definition property: (_) @name) @def.field
(function_declaration name: (identifier) @name) @def.func
(generator_function_declaration name: (identifier) @name) @def.func

; Top-level variables; a function value makes a function.
(program
  [(lexical_declaration (variable_declarator
     name: (identifier) @name
     value: [(arrow_function body: (_) @body) (function_expression body: (_) @body)]) @def.func)
   (variable_declaration (variable_declarator
     name: (identifier) @name
     value: [(arrow_function body: (_) @body) (function_expression body: (_) @body)]) @def.func)
   (export_statement (lexical_declaration (variable_declarator
     name: (identifier) @name
     value: [(arrow_function body: (_) @body) (function_expression body: (_) @body)]) @def.func))])
(program
  [(lexical_declaration "const" (variable_declarator name: (identifier) @name) @def.const)
   (export_statement (lexical_declaration "const" (variable_declarator name: (identifier) @name) @def.const))])
(program
  [(lexical_declaration (variable_declarator name: (identifier) @name) @def.var)
   (variable_declaration (variable_declarator name: (identifier) @name) @def.var)
   (export_statement [(lexical_declaration (variable_declarator name: (identifier) @name) @def.var)
                      (variable_declaration (variable_declarator name: (identifier) @name) @def.var)])])

; Imports.
(import_statement source: (string (string_fragment) @import.path)) @import
(import_statement
  (import_clause (identifier) @import.default)
  source: (string (string_fragment) @import.path)) @import
(import_statement
  (import_clause (namespace_import (identifier) @import.alias))
  source: (string (string_fragment) @import.path)) @import
(import_statement
  (import_clause (named_imports (import_specifier name: (_) @import.name alias: (_) @import.alias)))
  source: (string (string_fragment) @import.path)) @import
(import_statement
  (import_clause (named_imports (import_specifier name: (_) @import.name !alias)))
  source: (string (string_fragment) @import.path)) @import
(variable_declarator
  name: (identifier) @import.alias
  value: (call_expression
    function: (identifier) @_require
    arguments: (arguments . (string (string_fragment) @import.path)))
  (#eq? @_require "require")) @import

; Exports (plain exported declarations are public, not exports).
(export_statement
  (export_clause (export_specifier name: (_) @export.name alias: (_) @export.alias))
  source: (string (string_fragment) @export.source)) @export
(export_statement
  (export_clause (export_specifier name: (_) @export.name !alias))
  source: (string (string_fragment) @export.source)) @export
(export_statement
  (export_clause (export_specifier name: (_) @export.name alias: (_) @export.alias))
  !source) @export
(export_statement
  (export_clause (export_specifier name: (_) @export.name !alias))
  !source) @export
(export_statement "*" @export.all source: (string (string_fragment) @export.source)) @export
(export_statement value: (identifier) @export.default) @export

; References.
(call_expression function: (identifier) @name) @ref.call
(call_expression
  function: (member_expression
    object: (_) @ref.qualifier
    property: (property_identifier) @name)) @ref.call
(new_expression constructor: (identifier) @name) @ref.instantiate
(new_expression
  constructor: (member_expression object: (_) @ref.qualifier property: (property_identifier) @name)) @ref.instantiate
(class_heritage (identifier) @name) @ref.extends
(class_heritage (member_expression object: (_) @ref.qualifier property: (property_identifier) @name)) @ref.extends
(decorator (identifier) @name) @ref.decorator
(decorator (call_expression function: (identifier) @name)) @ref.decorator

; JSX: a capitalised element is a component.
(jsx_opening_element name: (identifier) @name (#match? @name "^[A-Z]")) @ref.instantiate
(jsx_self_closing_element name: (identifier) @name (#match? @name "^[A-Z]")) @ref.instantiate
