; Definitions. A docstring is the first statement of the body; the
; variant with @doc comes first so it wins.
(class_definition
  name: (identifier) @name
  body: (block . (string) @doc)) @def.class
(class_definition name: (identifier) @name) @def.class

(function_definition
  name: (identifier) @name
  body: (block . (string) @doc)) @def.func
(function_definition name: (identifier) @name) @def.func

(class_definition
  body: (block (assignment left: (identifier) @name) @def.field))
(module (assignment left: (identifier) @name) @def.var)

; Imports: one per imported module, with listed names.
(import_statement name: (dotted_name) @import.path @import)
(import_statement
  name: (aliased_import
    name: (dotted_name) @import.path
    alias: (identifier) @import.alias) @import)
(import_from_statement
  module_name: (_) @import.path
  name: (dotted_name) @import.name) @import
(import_from_statement
  module_name: (_) @import.path
  name: (aliased_import
    name: (dotted_name) @import.name
    alias: (identifier) @import.alias)) @import
(import_from_statement
  module_name: (_) @import.path
  (wildcard_import) @import.wildcard) @import

; __all__ lists the module's exports.
(module
  (assignment
    left: (identifier) @_all
    right: (list (string (string_content) @export.name))) @export
  (#eq? @_all "__all__"))

; References.
(call function: (identifier) @name) @ref.call
(call
  function: (attribute
    object: (_) @ref.qualifier
    attribute: (identifier) @name)) @ref.call

(class_definition superclasses: (argument_list (identifier) @name)) @ref.extends
(class_definition
  superclasses: (argument_list
    (attribute object: (_) @ref.qualifier attribute: (identifier) @name))) @ref.extends

(decorator (identifier) @name) @ref.decorator
(decorator (attribute object: (_) @ref.qualifier attribute: (identifier) @name)) @ref.decorator
(decorator (call function: (identifier) @name)) @ref.decorator
(decorator
  (call function: (attribute object: (_) @ref.qualifier attribute: (identifier) @name))) @ref.decorator
