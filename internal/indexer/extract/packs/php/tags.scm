(namespace_definition name: (namespace_name) @package)

; Definitions.
(class_declaration name: (name) @name body: (_) @body) @def.class
(interface_declaration name: (name) @name body: (_) @body) @def.interface
(trait_declaration name: (name) @name body: (_) @body) @def.trait
(enum_declaration name: (name) @name body: (_) @body) @def.enum
(enum_case name: (name) @name) @def.enum_member
(method_declaration name: (name) @name (#eq? @name "__construct")) @def.constructor
(method_declaration name: (name) @name !body) @def.method.decl
(method_declaration name: (name) @name) @def.method
(function_definition name: (name) @name) @def.func
(const_declaration (const_element (name) @name) @def.const)
(property_declaration (property_element name: (variable_name (name) @name)) @def.field)

; Imports: use A\B; use A\B as C; use A\{B, C as D};
(namespace_use_declaration
  (namespace_use_clause [(qualified_name) (name)] @import.path alias: (name) @import.alias) @import)
(namespace_use_declaration
  (namespace_use_clause [(qualified_name) (name)] @import.path !alias) @import)
(namespace_use_declaration
  (namespace_name) @import.path
  body: (namespace_use_group (namespace_use_clause (name) @import.name alias: (name) @import.alias))) @import
(namespace_use_declaration
  (namespace_name) @import.path
  body: (namespace_use_group (namespace_use_clause (name) @import.name !alias))) @import

; References.
(function_call_expression function: [(name) (qualified_name)] @name) @ref.call
(member_call_expression object: (_) @ref.qualifier name: (name) @name) @ref.call
(nullsafe_member_call_expression object: (_) @ref.qualifier name: (name) @name) @ref.call
(scoped_call_expression scope: (_) @ref.qualifier name: (name) @name) @ref.call
(object_creation_expression [(name) (qualified_name)] @name) @ref.instantiate
(base_clause [(name) (qualified_name)] @name) @ref.extends
(class_interface_clause [(name) (qualified_name)] @name) @ref.implements
(declaration_list (use_declaration [(name) (qualified_name)] @name) @ref.implements)
(attribute [(name) (qualified_name)] @name) @ref.decorator
