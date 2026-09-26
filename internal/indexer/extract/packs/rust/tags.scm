; Definitions. Functions in an impl block get the implemented type as
; receiver through the @scope.
(struct_item name: (type_identifier) @name) @def.struct
(union_item name: (type_identifier) @name) @def.struct
(enum_item name: (type_identifier) @name body: (_) @body) @def.enum
(enum_variant name: (identifier) @name) @def.enum_member
(trait_item name: (type_identifier) @name body: (_) @body) @def.trait
(function_item name: (identifier) @name) @def.func
(function_signature_item name: (identifier) @name) @def.func.decl
(field_declaration_list (field_declaration name: (field_identifier) @name) @def.field)
(mod_item name: (identifier) @name) @def.module
(const_item name: (identifier) @name) @def.const
(static_item name: (identifier) @name) @def.var
(type_item name: (type_identifier) @name) @def.type_alias
(macro_definition name: (identifier) @name (macro_rule) @body) @def.macro
(impl_item type: (_) @scope.name body: (_)) @scope

; Imports: use a::b::C, use a::b::{C, D as E}, use a::b::*, use a::B as C.
(use_declaration argument: [(scoped_identifier) (identifier)] @import.path) @import
(use_declaration
  argument: (use_as_clause path: (_) @import.path alias: (identifier) @import.alias)) @import
(use_declaration argument: (use_wildcard (_) @import.path) @import.wildcard) @import
(use_declaration
  argument: (scoped_use_list path: (_) @import.path list: (use_list (identifier) @import.name))) @import
(use_declaration
  argument: (scoped_use_list
    path: (_) @import.path
    list: (use_list (use_as_clause path: (_) @import.name alias: (identifier) @import.alias)))) @import

; pub use re-exports.
(use_declaration
  (visibility_modifier)
  argument: (scoped_identifier path: (_) @export.source name: (identifier) @export.name)) @export
(use_declaration
  (visibility_modifier)
  argument: (use_as_clause
    path: (scoped_identifier path: (_) @export.source name: (identifier) @export.name)
    alias: (identifier) @export.alias)) @export

; References.
(call_expression function: (identifier) @name) @ref.call
(call_expression function: (scoped_identifier path: (_) @ref.qualifier name: (identifier) @name)) @ref.call
(call_expression function: (field_expression value: (_) @ref.qualifier field: (field_identifier) @name)) @ref.call
(call_expression function: (generic_function function: (identifier) @name)) @ref.call
(call_expression
  function: (generic_function function: (scoped_identifier path: (_) @ref.qualifier name: (identifier) @name))) @ref.call
(macro_invocation macro: (identifier) @name) @ref.call
(struct_expression name: (type_identifier) @name) @ref.instantiate
(struct_expression name: (scoped_type_identifier path: (_) @ref.qualifier name: (type_identifier) @name)) @ref.instantiate
(impl_item trait: (type_identifier) @name) @ref.implements
(impl_item trait: (generic_type type: (type_identifier) @name)) @ref.implements
(impl_item trait: (scoped_type_identifier path: (_) @ref.qualifier name: (type_identifier) @name)) @ref.implements
(trait_item bounds: (trait_bounds (type_identifier) @name)) @ref.extends
