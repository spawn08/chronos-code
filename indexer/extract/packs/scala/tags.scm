(package_clause name: (package_identifier) @package)

; Definitions.
(class_definition name: (identifier) @name) @def.class
(object_definition name: (identifier) @name) @def.class
(trait_definition name: (identifier) @name) @def.trait
(enum_definition name: (identifier) @name) @def.enum
(type_definition name: (type_identifier) @name) @def.type_alias
(function_definition name: (identifier) @name) @def.func
(function_declaration name: (identifier) @name) @def.func.decl
(class_parameter (modifiers)? "val" name: (identifier) @name) @def.property
(template_body [(val_definition pattern: (identifier) @name) (var_definition pattern: (identifier) @name)] @def.field)
(template_body (val_declaration name: (identifier) @name) @def.field.decl)
(compilation_unit [(val_definition pattern: (identifier) @name) (var_definition pattern: (identifier) @name)] @def.var)

; Imports: import a.b.C, a.b.{C, D => E}, a.b._
(import_declaration path: (_) @import.path (namespace_wildcard) @import.wildcard) @import
(import_declaration path: (_) @import.path (namespace_selectors (identifier) @import.name)) @import
(import_declaration
  path: (_) @import.path
  (namespace_selectors (arrow_renamed_identifier name: (_) @import.name alias: (_) @import.alias))) @import
(import_declaration path: (_) @import.path) @import

; References: the first parent is extended, the rest are mixed in.
(call_expression function: (identifier) @name) @ref.call
(call_expression function: (field_expression value: (_) @ref.qualifier field: (identifier) @name)) @ref.call
(call_expression function: (generic_function function: (identifier) @name)) @ref.call
(call_expression
  function: (generic_function function: (field_expression value: (_) @ref.qualifier field: (identifier) @name))) @ref.call
(instance_expression (type_identifier) @name) @ref.instantiate
(instance_expression (generic_type type: (type_identifier) @name)) @ref.instantiate
(extends_clause . type: (type_identifier) @name) @ref.extends
(extends_clause . type: (generic_type type: (type_identifier) @name)) @ref.extends
(extends_clause type: (type_identifier) @name) @ref.implements
(extends_clause type: (generic_type type: (type_identifier) @name)) @ref.implements
(annotation name: (type_identifier) @name) @ref.decorator

; Type uses.
((type_identifier) @name @ref.type)

; Binding hints: typed parameters and values; new Foo(), Foo().
(parameter name: (identifier) @hint.name type: (_) @hint.type) @hint
(class_parameter name: (identifier) @hint.name type: (_) @hint.type) @hint
(val_definition pattern: (identifier) @hint.name type: (_) @hint.type) @hint
(var_definition pattern: (identifier) @hint.name type: (_) @hint.type) @hint
(val_definition pattern: (identifier) @hint.name value: (instance_expression (type_identifier) @hint.type)) @hint
(val_definition pattern: (identifier) @hint.name value: (call_expression function: (identifier) @hint.call)) @hint
