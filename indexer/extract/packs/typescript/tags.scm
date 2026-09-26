; Shared by the typescript and tsx packs: only use node types both
; grammars have.

; Definitions.
(class_declaration name: (type_identifier) @name body: (_) @body) @def.class
(abstract_class_declaration name: (type_identifier) @name body: (_) @body) @def.class
(interface_declaration name: (type_identifier) @name body: (_) @body) @def.interface
(type_alias_declaration name: (type_identifier) @name) @def.type_alias
(enum_declaration name: (identifier) @name body: (_) @body) @def.enum
(enum_body name: (property_identifier) @name @def.enum_member)
(enum_body (enum_assignment name: (property_identifier) @name) @def.enum_member)
(method_definition
  name: (property_identifier) @name
  (#eq? @name "constructor")) @def.constructor
(method_definition name: (_) @name) @def.method
(method_signature name: (_) @name) @def.method.decl
(abstract_method_signature name: (_) @name) @def.method.decl
(public_field_definition name: (_) @name) @def.field
(property_signature name: (_) @name) @def.property
(function_declaration name: (identifier) @name) @def.func
(generator_function_declaration name: (identifier) @name) @def.func
(function_signature name: (identifier) @name) @def.func.decl
(internal_module name: (_) @name body: (_) @body) @def.namespace
(module name: (string (string_fragment) @name) body: (_) @body) @def.module
(module name: (identifier) @name body: (_) @body) @def.module

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
(extends_clause value: (identifier) @name) @ref.extends
(extends_clause
  value: (member_expression object: (_) @ref.qualifier property: (property_identifier) @name)) @ref.extends
(extends_type_clause type: (type_identifier) @name) @ref.extends
(extends_type_clause type: (generic_type name: (type_identifier) @name)) @ref.extends
(implements_clause (type_identifier) @name) @ref.implements
(implements_clause (generic_type name: (type_identifier) @name)) @ref.implements
(decorator (identifier) @name) @ref.decorator
(decorator (call_expression function: (identifier) @name)) @ref.decorator
(decorator
  (call_expression function: (member_expression object: (_) @ref.qualifier property: (property_identifier) @name))) @ref.decorator

; Type uses.
(nested_type_identifier module: (_) @ref.qualifier name: (type_identifier) @name) @ref.type
((type_identifier) @name @ref.type (#not-has-parent? @name nested_type_identifier))

; Binding hints: typed parameters, variables and fields; x = new Foo().
(required_parameter pattern: (identifier) @hint.name type: (type_annotation (_) @hint.type)) @hint
(optional_parameter pattern: (identifier) @hint.name type: (type_annotation (_) @hint.type)) @hint
(variable_declarator name: (identifier) @hint.name type: (type_annotation (_) @hint.type)) @hint
(variable_declarator
  name: (identifier) @hint.name
  value: (new_expression constructor: [(identifier) (member_expression)] @hint.type)) @hint
(public_field_definition name: (_) @hint.name type: (type_annotation (_) @hint.type)) @hint
(public_field_definition
  name: (_) @hint.name
  value: (new_expression constructor: [(identifier) (member_expression)] @hint.type)) @hint
(assignment_expression
  left: (member_expression) @hint.name
  right: (new_expression constructor: [(identifier) (member_expression)] @hint.type)) @hint
