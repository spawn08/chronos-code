; Types and namespaces.
(namespace_definition name: (_) @name body: (_) @body) @def.namespace
(class_specifier name: (type_identifier) @name body: (_) @body) @def.class
(struct_specifier name: (type_identifier) @name body: (_) @body) @def.struct
(union_specifier name: (type_identifier) @name body: (_) @body) @def.struct
(enum_specifier name: (type_identifier) @name body: (_) @body) @def.enum
(enumerator name: (identifier) @name) @def.enum_member
(type_definition declarator: (type_identifier) @name) @def.type_alias
(alias_declaration name: (type_identifier) @name) @def.type_alias
(preproc_def name: (identifier) @name) @def.macro
(preproc_function_def name: (identifier) @name) @def.macro

; Out-of-line members: Repo::Repo() is a constructor, Repo::find a method.
(function_definition
  declarator: (function_declarator
    declarator: (qualified_identifier scope: (_) @receiver name: (identifier) @name))
  (#eq? @receiver @name)) @def.constructor
(function_definition
  declarator: (function_declarator
    declarator: (qualified_identifier scope: (_) @receiver name: (_) @name))) @def.method
(function_definition
  declarator: (reference_declarator (function_declarator
    declarator: (qualified_identifier scope: (_) @receiver name: (_) @name)))) @def.method
(function_definition
  declarator: (pointer_declarator declarator: (function_declarator
    declarator: (qualified_identifier scope: (_) @receiver name: (_) @name)))) @def.method

; Functions, and member functions defined in the class body.
(function_definition declarator: (function_declarator declarator: [(identifier) (field_identifier) (operator_name) (destructor_name)] @name)) @def.func
(function_definition
  declarator: [
    (pointer_declarator declarator: (function_declarator declarator: [(identifier) (field_identifier) (operator_name)] @name))
    (reference_declarator (function_declarator declarator: [(identifier) (field_identifier) (operator_name)] @name))]) @def.func

; Member declarations in a class body.
(field_declaration_list
  (declaration declarator: (function_declarator declarator: (identifier) @name)) @def.constructor.decl)
(field_declaration_list
  (declaration declarator: (function_declarator declarator: (destructor_name) @name)) @def.method.decl)
(field_declaration_list
  (field_declaration declarator: (function_declarator declarator: [(field_identifier) (operator_name)] @name)) @def.method.decl)
(field_declaration_list
  (field_declaration
    declarator: [
      (pointer_declarator declarator: (function_declarator declarator: [(field_identifier) (operator_name)] @name))
      (reference_declarator (function_declarator declarator: [(field_identifier) (operator_name)] @name))]) @def.method.decl)
(field_declaration_list
  (field_declaration declarator: [
    (field_identifier) @name @def.field
    (pointer_declarator declarator: (field_identifier) @name) @def.field
    (reference_declarator (field_identifier) @name) @def.field
    (array_declarator declarator: (field_identifier) @name) @def.field]))

; Free function prototypes and variables at namespace level.
([(translation_unit) (declaration_list)]
  (declaration declarator: (function_declarator declarator: [(identifier) (qualified_identifier)] @name)) @def.func.decl)
([(translation_unit) (declaration_list)]
  (declaration declarator: [
    (identifier) @name @def.var
    (init_declarator declarator: (identifier) @name) @def.var
    (pointer_declarator declarator: (identifier) @name) @def.var
    (init_declarator declarator: (pointer_declarator declarator: (identifier) @name)) @def.var]))

; Includes.
(preproc_include path: (_) @import.path) @include
(using_declaration "namespace" (_) @import.path @import.wildcard) @import

; References.
(call_expression function: (identifier) @name) @ref.call
(call_expression function: (qualified_identifier scope: (_) @ref.qualifier name: (identifier) @name)) @ref.call
(call_expression function: (field_expression argument: (_) @ref.qualifier field: (field_identifier) @name)) @ref.call
(call_expression function: (template_function name: (identifier) @name)) @ref.call
(call_expression
  function: (qualified_identifier scope: (_) @ref.qualifier name: (template_function name: (identifier) @name))) @ref.call
(new_expression type: (type_identifier) @name) @ref.instantiate
(new_expression type: (qualified_identifier scope: (_) @ref.qualifier name: (type_identifier) @name)) @ref.instantiate
(new_expression type: (template_type name: (type_identifier) @name)) @ref.instantiate
(base_class_clause (type_identifier) @name) @ref.extends
(base_class_clause (qualified_identifier scope: (_) @ref.qualifier name: (type_identifier) @name)) @ref.extends
(base_class_clause (template_type name: (type_identifier) @name)) @ref.extends

; Type uses.
(qualified_identifier scope: (_) @ref.qualifier name: (type_identifier) @name) @ref.type
((type_identifier) @name @ref.type)

; Binding hints: typed parameters, variables and fields; auto x = new T().
(parameter_declaration
  type: (_) @hint.type
  declarator: [
    (identifier) @hint.name
    (pointer_declarator declarator: (identifier) @hint.name)
    (reference_declarator (identifier) @hint.name)]) @hint
(declaration
  type: (_) @hint.type
  declarator: [
    (identifier) @hint.name
    (init_declarator declarator: (identifier) @hint.name)
    (pointer_declarator declarator: (identifier) @hint.name)
    (init_declarator declarator: (pointer_declarator declarator: (identifier) @hint.name))]) @hint
(declaration
  declarator: (init_declarator declarator: (identifier) @hint.name value: (new_expression type: (_) @hint.type))) @hint
(field_declaration
  type: (_) @hint.type
  declarator: [(field_identifier) @hint.name (pointer_declarator declarator: (field_identifier) @hint.name)]) @hint
