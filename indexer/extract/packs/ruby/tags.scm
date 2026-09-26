; Definitions.
(class name: [(constant) (scope_resolution)] @name) @def.class
(module name: [(constant) (scope_resolution)] @name) @def.module
(method name: (_) @name) @def.method
(singleton_method name: (_) @name) @def.method
(assignment left: (constant) @name) @def.const
(call
  method: (identifier) @_attr
  arguments: (argument_list (simple_symbol) @name @def.property)
  (#match? @_attr "^attr_(reader|writer|accessor)$"))

; require and require_relative.
(call
  method: (identifier) @_require
  arguments: (argument_list . (string (string_content) @import.path))
  (#match? @_require "^require(_relative)?$")) @import

; References: include/extend/prepend mix a module in.
(call
  method: (identifier) @_mixin
  arguments: (argument_list [(constant) (scope_resolution)] @name)
  (#match? @_mixin "^(include|extend|prepend)$")) @ref.implements
(call receiver: (constant) @name method: (identifier) @_new (#eq? @_new "new")) @ref.instantiate
(call !receiver method: (identifier) @name) @ref.call
(call receiver: (_) @ref.qualifier method: (identifier) @name) @ref.call
(superclass [(constant) (scope_resolution)] @name) @ref.extends

; Binding hints: x = Foo.new.
(assignment
  left: [(identifier) (instance_variable)] @hint.name
  right: (call receiver: [(constant) (scope_resolution)] @hint.type method: (identifier) @_new)
  (#eq? @_new "new")) @hint
